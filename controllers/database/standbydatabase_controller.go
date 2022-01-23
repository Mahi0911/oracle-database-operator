/*
** Copyright (c) 2021 Oracle and/or its affiliates.
**
** The Universal Permissive License (UPL), Version 1.0
**
** Subject to the condition set forth below, permission is hereby granted to any
** person obtaining a copy of this software, associated documentation and/or data
** (collectively the "Software"), free of charge and under any and all copyright
** rights in the Software, and any and all patent rights owned or freely
** licensable by each licensor hereunder covering either (i) the unmodified
** Software as contributed to or provided by such licensor, or (ii) the Larger
** Works (as defined below), to deal in both
**
** (a) the Software, and
** (b) any piece of software and/or hardware listed in the lrgrwrks.txt file if
** one is included with the Software (each a "Larger Work" to which the Software
** is contributed by such licensors),
**
** without restriction, including without limitation the rights to copy, create
** derivative works of, display, perform, and distribute the Software and make,
** use, sell, offer for sale, import, export, have made, and have sold the
** Software and the Larger Work(s), and to sublicense the foregoing rights on
** either these or other terms.
**
** This license is subject to the following condition:
** The above copyright notice and either this complete permission notice or at
** a minimum a reference to the UPL must be included in all copies or
** substantial portions of the Software.
**
** THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
** IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
** FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
** AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
** LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
** OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
** SOFTWARE.
 */

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	dbapi "github.com/oracle/oracle-database-operator/apis/database/v1alpha1"
	dbcommons "github.com/oracle/oracle-database-operator/commons/database"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// StandbyDatabaseReconciler reconciles a StandbyDatabase object
type StandbyDatabaseReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Config   *rest.Config
	Recorder record.EventRecorder
}

const standbyDatabaseFinalizer = "database.oracle.com/standbyfinalizer"

//+kubebuilder:rbac:groups=database.oracle.com,resources=standbydatabases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=database.oracle.com,resources=standbydatabases/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=database.oracle.com,resources=standbydatabases/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods;pods/log;pods/exec;persistentvolumeclaims;services;nodes;events,verbs=create;delete;get;list;patch;update;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the StandbyDatabase object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *StandbyDatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	r.Log.Info("Reconcile requested")

	standbyDatabase := &dbapi.StandbyDatabase{}
	err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, standbyDatabase)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Log.Info("Resource deleted")
			return requeueN, nil
		}
		return requeueN, err
	}

	// Manage StandbyDatabase Deletion
	result := r.manageStandbyDatabaseDeletion(req, ctx, standbyDatabase)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Fetch Primary Database Reference
	singleInstanceDatabase := &dbapi.SingleInstanceDatabase{}
	err = r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: standbyDatabase.Spec.PrimaryDatabaseRef}, singleInstanceDatabase)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Log.Info("Resource deleted")
			return requeueN, nil
		}
		return requeueN, err
	}

	// Always refresh status before a reconcile
	defer r.Status().Update(ctx, standbyDatabase)

	// Validate if Primary Database Reference is ready and Database modes are ON
	result, sidbReadyPod := r.validateSidbReadiness(standbyDatabase, singleInstanceDatabase, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Setup Primary Database Reference before creating standby
	result = r.checkPrerequisites(standbyDatabase, singleInstanceDatabase, sidbReadyPod, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Service creation
	result = r.createSVC(ctx, req, standbyDatabase)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// PVC creation
	result = r.createPVC(ctx, req, standbyDatabase)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Create Standby Database Pods , Scale In and Out
	result = r.createPods(standbyDatabase, singleInstanceDatabase, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	if standbyDatabase.Status.DatafilesCreated != "true" {
		// Creation of Oracle Wallet for Standby Database credentials
		result, err = r.createWallet(standbyDatabase, singleInstanceDatabase, ctx, req)
		if result.Requeue {
			r.Log.Info("Reconcile queued")
			return result, nil
		}
		if err != nil {
			r.Log.Info("Spec validation failed")
			return result, nil
		}
	}

	// Validate the Standby Database Readiness
	result, readyPod := r.validateDBReadiness(standbyDatabase, singleInstanceDatabase, sidbReadyPod, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Post DB ready operations

	// Deleting the oracle wallet
	if standbyDatabase.Status.DatafilesCreated == "true" {
		result, err = r.deleteWallet(standbyDatabase, singleInstanceDatabase, ctx, req)
		if result.Requeue {
			r.Log.Info("Reconcile queued")
			return result, nil
		}
	}

	// update status to Ready after all operations succeed
	standbyDatabase.Status.Status = dbcommons.StatusReady
	r.updateReconcileStatus(standbyDatabase, singleInstanceDatabase, readyPod, ctx, req)

	r.Log.Info("Reconcile completed")
	return ctrl.Result{}, nil

}

//#####################################################################################################
//    Validate Readiness of the primary DB specified
//#####################################################################################################
func (r *StandbyDatabaseReconciler) validateSidbReadiness(m *dbapi.StandbyDatabase,
	n *dbapi.SingleInstanceDatabase, ctx context.Context, req ctrl.Request) (ctrl.Result, corev1.Pod) {

	log := r.Log.WithValues("validateSidbReadiness", req.NamespacedName)

	// ## FETCH THE SIDB REPLICAS .
	sidbReadyPod, _, _, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		n.Spec.Image.PullFrom, n.Name, n.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, sidbReadyPod
	}

	if n.Status.Status != dbcommons.StatusReady {
		m.Status.Status = dbcommons.StatusPending
		eventReason := "Waiting"
		eventMsg := "Waiting for " + n.Name + " to be Ready"
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
		return requeueY, sidbReadyPod
	}

	// Check if primaryDatabaseRef role is PRIMARY
	databaseRole, err := dbcommons.GetDatabaseRole(sidbReadyPod, r, r.Config, ctx, req, n.Spec.Edition)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, sidbReadyPod
	}

	// Once standby datafiles created , need not wait for sidb to be primary(m.Status.DatafilesCreated != "true")
	if m.Status.DatafilesCreated != "true" && strings.ToUpper(databaseRole) != "PRIMARY" {
		m.Status.Status = dbcommons.StatusPending
		eventReason := "Waiting"
		eventMsg := "Waiting for " + n.Name + " to be PRIMARY"
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

		return requeueY, sidbReadyPod
	}

	// Validate databaseRef Admin Password
	adminPasswordSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: m.Spec.AdminPassword.SecretName, Namespace: m.Namespace}, adminPasswordSecret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			m.Status.Status = dbcommons.StatusError
			eventReason := "Waiting"
			eventMsg := "waiting for secret : " + m.Spec.AdminPassword.SecretName + " to get created"
			r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
			r.Log.Info("Secret " + m.Spec.AdminPassword.SecretName + " Not Found")
			return requeueY, sidbReadyPod
		}
		log.Error(err, err.Error())
		return requeueY, sidbReadyPod
	}
	adminPassword := string(adminPasswordSecret.Data[m.Spec.AdminPassword.SecretKey])

	out, err := dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, true, "bash", "-c",
		fmt.Sprintf("echo -e  \"%s\"  | %s", fmt.Sprintf(dbcommons.ValidateAdminPassword, adminPassword), dbcommons.GetSqlClient(n.Spec.Edition)))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, sidbReadyPod
	}
	if strings.Contains(out, "USER is \"SYS\"") {
		log.Info("validated Admin password successfully")
	} else if strings.Contains(out, "ORA-01017") {
		m.Status.Status = dbcommons.StatusError
		eventReason := "Logon denied"
		eventMsg := "invalid databaseRef admin password. secret: " + m.Spec.AdminPassword.SecretName
		r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, eventMsg)
		return requeueY, sidbReadyPod
	} else {
		return requeueY, sidbReadyPod
	}

	flashBackStatus, archiveLogStatus, forceLoggingStatus, result := dbcommons.CheckDBConfig(sidbReadyPod, r, r.Config, ctx, req, n.Spec.Edition)
	if result.Requeue {
		return result, sidbReadyPod
	}
	if !flashBackStatus || !archiveLogStatus || !forceLoggingStatus {
		m.Status.Status = dbcommons.StatusPending
		eventReason := "Waiting"
		eventMsg := ""
		if !n.Spec.ArchiveLog || !n.Spec.FlashBack || !n.Spec.ForceLogging {
			eventMsg = "Turn ON Flashback,ForceLogging,ArchiveLog on " + n.Name
		} else {
			eventMsg = "Waiting for Flashback,ForceLogging to turn ON , ArchiveLog to True on " + n.Name
		}
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
		log.Info(eventMsg)
		return requeueY, sidbReadyPod
	}

	return requeueN, sidbReadyPod
}

//#####################################################################################################
//    Check prerequisites of the primary DB specified
//#####################################################################################################
func (r *StandbyDatabaseReconciler) checkPrerequisites(m *dbapi.StandbyDatabase,
	n *dbapi.SingleInstanceDatabase, sidbReadyPod corev1.Pod, ctx context.Context, req ctrl.Request) ctrl.Result {

	log := r.Log.WithValues("checkPrerequisites", req.NamespacedName)

	// ## FETCH THE STANDBY REPLICAS .
	_, replicasFound, _, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		n.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}

	if replicasFound > 0 {
		return requeueN
	}
	// ## MODIFYING Tnsnames.ora FOR SIDB DB ##
	out, err := dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "",
		ctx, req, false, "bash", "-c", fmt.Sprintf("cat /opt/oracle/oradata/dbconfig/%s/tnsnames.ora", strings.ToUpper(n.Spec.Sid)))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	} else {
		log.Info("tnsnames.ora Output")
		log.Info(out)
	}

	if strings.Contains(out, "(SERVICE_NAME = "+strings.ToUpper(m.Spec.Sid)+")") {
		log.Info("TNS ENTRY OF " + m.Spec.Sid + " ALREADY EXISTS ON SIDB ")
	} else {

		tnsnamesEntry := dbcommons.StandbyTnsnamesEntry
		tnsnamesEntry = strings.ReplaceAll(tnsnamesEntry, "##STANDBYDATABASE_SID##", m.Spec.Sid)
		tnsnamesEntry = strings.ReplaceAll(tnsnamesEntry, "##STANDBYDATABASE_SERVICE_EXPOSED##", m.Name)

		// ## MODIFYING Tnsnames.ora FOR SIDB ##
		out, err = dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | cat >> /opt/oracle/oradata/dbconfig/%s/tnsnames.ora ", tnsnamesEntry, strings.ToUpper(n.Spec.Sid)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		} else {
			log.Info("Modifying tnsnames.ora Output")
			log.Info(out)
		}

	}
	// ## MODIFYING Listener.ora FOR SIDB ##
	out, err = dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "",
		ctx, req, false, "bash", "-c", fmt.Sprintf("cat /opt/oracle/oradata/dbconfig/%s/listener.ora ", strings.ToUpper(n.Spec.Sid)))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	} else {
		log.Info("listener.ora Output")
		log.Info(out)
	}
	if strings.Contains(out, strings.ToUpper(n.Spec.Sid)+"_DGMGRL") {
		log.Info("LISTENER.ORA ALREADY HAS " + n.Spec.Sid + "_DGMGRL ENTRY IN SID_LIST_LISTENER ")
	} else {
		// ## MODIFYING Listener.ora FOR SIDB ##
		out, err = dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | cat > /opt/oracle/oradata/dbconfig/%s/listener.ora ", dbcommons.ListenerEntry, strings.ToUpper(n.Spec.Sid)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		} else {
			log.Info("Modifying listener.ora Output")
			log.Info(out)
		}
		// ## STOPING LISTENER OF SIDB POD ##
		out, err = dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "",
			ctx, req, false, "bash", "-c", "lsnrctl stop")
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		} else {
			log.Info("lsnrctl stop Output")
			log.Info(out)
		}

		// ## STARTING LISTENER OF SIDB POD ##
		out, err = dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "",
			ctx, req, false, "bash", "-c", "lsnrctl start")
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		} else {
			log.Info("lsnrctl start Output")
			log.Info(out)
		}

	}

	// ## standbyDatabase PREREQUISITES FOR SIDB POD ##
	out, err = dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
		fmt.Sprintf("echo -e  \"%s\"  | sqlplus -s / as sysdba", dbcommons.StandbyDatabasePrerequisitesSQL))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	} else {
		log.Info("standbyDatabasePrerequisites Output")
		log.Info(out)
	}

	return requeueN
}

//#############################################################################
//    Instantiate POD spec from StandbyDatabase spec
//#############################################################################
func (r *StandbyDatabaseReconciler) instantiatePodSpec(m *dbapi.StandbyDatabase,
	n *dbapi.SingleInstanceDatabase) *corev1.Pod {

	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind: "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name + "-" + dbcommons.GenerateRandomString(5),
			Namespace: m.Namespace,
			Labels: map[string]string{
				"app":     m.Name,
				"version": n.Spec.Image.Version,
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "datamount",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: m.Name,
						ReadOnly:  false,
					},
				},
			}},
			InitContainers: []corev1.Container{{
				Name:    "init-permissions",
				Image:   n.Spec.Image.PullFrom,
				Command: []string{"/bin/sh", "-c", fmt.Sprintf("chown %d:%d /opt/oracle/oradata", int(dbcommons.ORACLE_UID), int(dbcommons.ORACLE_GUID))},
				SecurityContext: &corev1.SecurityContext{
					// User ID 0 means, root user
					RunAsUser: func() *int64 { i := int64(0); return &i }(),
				},
				VolumeMounts: []corev1.VolumeMount{{
					MountPath: "/opt/oracle/oradata",
					Name:      "datamount",
				}},
			}, {
				Name:  "init-wallet",
				Image: n.Spec.Image.PullFrom,
				Env: []corev1.EnvVar{
					{
						Name:  "ORACLE_SID",
						Value: strings.ToUpper(m.Spec.Sid),
					},
					{
						Name:  "WALLET_CLI",
						Value: "mkstore",
					},
					{
						Name:  "WALLET_DIR",
						Value: "/opt/oracle/oradata/dbconfig/$(ORACLE_SID)/.wallet",
					},
				},
				Command: []string{"/bin/sh"},
				Args: func() []string {
					edition := n.Spec.Edition
					if n.Spec.Edition == "" {
						edition = "enterprise"
					}
					return []string{"-c", fmt.Sprintf(dbcommons.InitWalletCMD, edition)}
				}(),
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:  func() *int64 { i := int64(dbcommons.ORACLE_UID); return &i }(),
					RunAsGroup: func() *int64 { i := int64(dbcommons.ORACLE_GUID); return &i }(),
				},
				VolumeMounts: []corev1.VolumeMount{{
					MountPath: "/opt/oracle/oradata",
					Name:      "datamount",
				}},
			}},
			Containers: []corev1.Container{{
				Name:  m.Name,
				Image: n.Spec.Image.PullFrom,
				Lifecycle: &corev1.Lifecycle{
					PreStop: &corev1.Handler{
						Exec: &corev1.ExecAction{
							Command: []string{"/bin/sh", "-c", "/bin/echo -en 'shutdown abort;\n' | env ORACLE_SID=${ORACLE_SID^^} sqlplus -S / as sysdba"},
						},
					},
				},
				ImagePullPolicy: corev1.PullAlways,
				Ports:           []corev1.ContainerPort{{ContainerPort: 1521}, {ContainerPort: 5500}},

				ReadinessProbe: &corev1.Probe{
					Handler: corev1.Handler{
						Exec: &corev1.ExecAction{
							Command: []string{"/bin/sh", "-c", "$ORACLE_BASE/checkDBLockStatus.sh"},
						},
					},
					InitialDelaySeconds: 20,
					TimeoutSeconds:      20,
					PeriodSeconds:       40,
				},

				VolumeMounts: []corev1.VolumeMount{{
					MountPath: "/opt/oracle/oradata",
					Name:      "datamount",
				}},
				Env: []corev1.EnvVar{
					{
						Name:  "SVC_HOST",
						Value: m.Name,
					},
					{
						Name:  "SVC_PORT",
						Value: "1521",
					},
					{
						Name:  "ORACLE_SID",
						Value: strings.ToUpper(m.Spec.Sid),
					},
					{
						Name:  "PRIMARY_DB_CONN_STR",
						Value: n.Name + ":1521/" + strings.ToUpper(n.Spec.Sid),
					},
					{
						Name:  "PRIMARY_SID",
						Value: strings.ToUpper(n.Spec.Sid),
					},
					{
						Name:  "PRIMARY_IP",
						Value: n.Name,
					},
					{
						Name: "CREATE_PDB",
						Value: func() string {
							if n.Spec.Pdbname != "" {
								return "true"
							}
							return "false"
						}(),
					},
					{
						Name: "ORACLE_HOSTNAME",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "status.podIP",
							},
						},
					},
					{
						Name:  "STANDBY_DB",
						Value: "true",
					},
					{
						Name:  "WALLET_DIR",
						Value: "/opt/oracle/oradata/dbconfig/$(ORACLE_SID)/.wallet",
					},
				},
			}},

			TerminationGracePeriodSeconds: func() *int64 { i := int64(30); return &i }(),

			NodeSelector: func() map[string]string {
				ns := make(map[string]string)
				if len(m.Spec.NodeSelector) != 0 {
					for key, value := range m.Spec.NodeSelector {
						ns[key] = value
					}
				}
				return ns
			}(),

			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: func() *int64 { i := int64(54321); return &i }(),

				FSGroup: func() *int64 { i := int64(54321); return &i }(),
			},
			ImagePullSecrets: []corev1.LocalObjectReference{
				{
					Name: n.Spec.Image.PullSecrets,
				},
			},
		},
	}

	// Set StandbyDatabase instance as the owner and controller
	ctrl.SetControllerReference(m, pod, r.Scheme)
	return pod
}

//#############################################################################
//    Instantiate Service spec from StandbyDatabase spec
//#############################################################################
func (r *StandbyDatabaseReconciler) instantiateSVCSpec(m *dbapi.StandbyDatabase) *corev1.Service {
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind: "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"app": m.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:     "listener",
					Port:     1521,
					Protocol: corev1.ProtocolTCP,
				},
				{
					Name:     "xmldb",
					Port:     5500,
					Protocol: corev1.ProtocolTCP,
				},
			},
			Selector: map[string]string{
				"app": m.Name,
			},
			Type: "NodePort",
		},
	}
	// Set StandbyDatabase instance as the owner and controller
	ctrl.SetControllerReference(m, svc, r.Scheme)
	return svc
}

//#############################################################################
//    Instantiate Persistent Volume Claim spec from StandbyDatabase spec
//#############################################################################
func (r *StandbyDatabaseReconciler) instantiatePVCSpec(m *dbapi.StandbyDatabase) *corev1.PersistentVolumeClaim {

	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			Kind: "PersistentVolumeClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"app": m.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: func() []corev1.PersistentVolumeAccessMode {
				var accessMode []corev1.PersistentVolumeAccessMode
				accessMode = append(accessMode, corev1.PersistentVolumeAccessMode(m.Spec.Persistence.AccessMode))
				return accessMode
			}(),
			Resources: corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{
					// Requests describes the minimum amount of compute resources required
					"storage": resource.MustParse(m.Spec.Persistence.Size),
				},
			},
			StorageClassName: &m.Spec.Persistence.StorageClass,
		},
	}
	// Set StandbyDatabase instance as the owner and controller
	ctrl.SetControllerReference(m, pvc, r.Scheme)
	return pvc
}

//#############################################################################
//    Create a Service for StandbyDatabase
//#############################################################################
func (r *StandbyDatabaseReconciler) createSVC(ctx context.Context, req ctrl.Request,
	m *dbapi.StandbyDatabase) ctrl.Result {

	log := r.Log.WithValues("createSVC", req.NamespacedName)

	// Check if the Service already exists, if not create a new one
	svc := &corev1.Service{}
	// Get retrieves an obj for the given object key from the Kubernetes Cluster.
	// obj must be a struct pointer so that obj can be updated with the response returned by the Server.
	// Here foundsvc is the struct pointer to corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, svc)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new Service
		svc = r.instantiateSVCSpec(m)
		log.Info("Creating a new Service", "Service.Namespace", svc.Namespace, "Service.Name", svc.Name)
		err = r.Create(ctx, svc)
		if err != nil {
			log.Error(err, "Failed to create new Service", "Service.Namespace", svc.Namespace, "Service.Name", svc.Name)
			return requeueY
		}
		timeout := 30
		// Waiting for Service to get created as sometimes it takes some time to create a service . 30 seconds TImeout
		err = dbcommons.WaitForStatusChange(r, svc.Name, m.Namespace, ctx, req, time.Duration(timeout)*time.Second, "svc", "creation")
		if err != nil {
			log.Error(err, "Error in Waiting for svc status for Creation", "svc.Namespace", svc.Namespace, "SVC.Name", svc.Name)
			return requeueY
		}
		log.Info("Succesfully Created New Service ", "Service.Name : ", svc.Name)

	} else if err != nil {
		log.Error(err, "Failed to get Service")
		return requeueY
	}
	log.Info(" ", "Found Existing Service ", svc.Name)

	nodeip := dbcommons.GetNodeIp(r, ctx, req)

	m.Status.ClusterConnectString = svc.Name + "." + svc.Namespace + ":" + fmt.Sprint(svc.Spec.Ports[0].Port) + "/" + strings.ToUpper(m.Spec.Sid)
	m.Status.ExternalConnectString = nodeip + ":" + fmt.Sprint(svc.Spec.Ports[0].NodePort) + "/" + strings.ToUpper(m.Spec.Sid)

	return requeueN
}

//#############################################################################
//    Stake a claim for Persistent Volume
//#############################################################################
func (r *StandbyDatabaseReconciler) createPVC(ctx context.Context, req ctrl.Request,
	m *dbapi.StandbyDatabase) ctrl.Result {

	log := r.Log.WithValues("createPVC", req.NamespacedName)

	//  If Block Volume , Ensure Replicas=1
	if m.Spec.Persistence.AccessMode == "ReadWriteOnce" && m.Spec.Replicas != 1 {
		eventReason := "Spec Error"
		eventMsg := " ReadWriteOnce supports only one replica"
		r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, eventMsg)
		return requeueY
	}

	// Check if the PVC already exists using r.Get, if not create a new one using r.Create
	pvc := &corev1.PersistentVolumeClaim{}
	// Get retrieves an obj for the given object key from the Kubernetes Cluster.
	// obj must be a struct pointer so that obj can be updated with the response returned by the Server.
	// Here foundpvc is the struct pointer to corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, pvc)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new PVC
		pvc = r.instantiatePVCSpec(m)
		log.Info("Creating a new PVC", "PVC.Namespace", pvc.Namespace, "PVC.Name", pvc.Name)
		err = r.Create(ctx, pvc)
		if err != nil {
			log.Error(err, "Failed to create new PVC", "PVC.Namespace", pvc.Namespace, "PVC.Name", pvc.Name)
			return requeueY
		}
		timeout := 30
		// Waiting for PVC to get created as sometimes it takes some time to create a PVC . 30 seconds TImeout
		err = dbcommons.WaitForStatusChange(r, pvc.Name, m.Namespace, ctx, req, time.Duration(timeout)*time.Second, "pvc", "creation")
		if err != nil {
			log.Error(err, "Error in Waiting for Pvc status for Creation", "Pvc.Namespace", pvc.Namespace, "PVC.Name", pvc.Name)
			return requeueY
		}
		log.Info("Succesfully Created New PVC ", "PVC.Name : ", pvc.Name)

	} else if err != nil {
		log.Error(err, "Failed to get PVC")
		return requeueY
	}
	log.Info("", "Found Existing PVC ", pvc.Name)

	return requeueN
}

//#############################################################################
//    Create the requested POD replicas
//#############################################################################
func (r *StandbyDatabaseReconciler) createPods(m *dbapi.StandbyDatabase,
	n *dbapi.SingleInstanceDatabase, ctx context.Context, req ctrl.Request) ctrl.Result {

	log := r.Log.WithValues("createPods", req.NamespacedName)

	readyPod, replicasFound, available, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		(n.Spec.Image.PullFrom), m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	if replicasFound == 0 {
		m.Status.DatafilesCreated = "false"
		// Store all standbyDatabase sid:name in a map to use it during manual switchover.
		if len(n.Status.StandbyDatabases) == 0 {
			n.Status.StandbyDatabases = make(map[string]string)
		}
		n.Status.StandbyDatabases[strings.ToUpper(m.Spec.Sid)] = m.Name
		r.Status().Update(ctx, n)
	}

	replicasReq := m.Spec.Replicas
	if replicasFound == replicasReq {
		log.Info("No of " + m.Name + " replicas Found are same as Required")
	} else {
		//  if Found < Required , Create New Pods , Name of Pods are generated Randomly
		for i := replicasFound; i < replicasReq; i++ {
			pod := r.instantiatePodSpec(m, n)
			log.Info("Creating a new "+m.Name+" POD", "POD.Namespace", pod.Namespace, "POD.Name", pod.Name)
			err := r.Create(ctx, pod)
			if err != nil {
				log.Error(err, "Failed to create new "+m.Name+" POD", "pod.Namespace", pod.Namespace, "POD.Name", pod.Name)
				return requeueY
			}
			timeout := 30
			// Waiting for Pod to get created as sometimes it takes some time to create a Pod . 30 seconds TImeout
			err = dbcommons.WaitForStatusChange(r, pod.Name, m.Namespace, ctx, req, time.Duration(timeout)*time.Second, "pod", "creation")
			if err != nil {
				log.Error(err, "Error in Waiting for Pod status for Creation", "pod.Namespace", pod.Namespace, "POD.Name", pod.Name)
				return requeueY
			}
			log.Info("Succesfully Created new "+m.Name+" POD", "POD.NAME : ", pod.Name)

		}

		//  if Found > Required , Delete Extra Pods
		noOfPodsAvailable := replicasFound // No of Pods which are avaiable to delete
		noOfPodsRequired := replicasReq    // No of Replicas to be left after Deletion (other than Ready pod)

		if readyPod.Name != "" {
			noOfPodsAvailable = noOfPodsAvailable - 1 // Ready Pod cant be deleted
			noOfPodsRequired = noOfPodsRequired - 1   // Ready pod already exists . So Pods Required (other than Ready pod) will be 1 less
		}
		for i := noOfPodsAvailable; i > noOfPodsRequired; i-- {
			log.Info("Deleting "+m.Name+" Pod : ", "POD.NAME", available[i-1].Name)
			err := r.Delete(ctx, &available[i-1], &client.DeleteOptions{})

			if err != nil {
				log.Error(err, "Failed to Delete existing "+m.Name+" POD", "pod.Namespace", available[i-1].Namespace, "POD.Name", available[i-1].Name)
				return requeueY
			}
			//Waiting for Pod to change its status . 30 Second timeout
			timeout := 30
			err = dbcommons.WaitForStatusChange(r, available[i-1].Name, m.Namespace, ctx, req, time.Duration(timeout)*time.Second, "pod", "deletion")
			if err != nil {
				log.Error(err, "Error in Waiting for "+m.Name+" Pod status to Terminate", "pod.Namespace", available[i-1].Namespace, "POD.Name", available[i-1].Name)
				return requeueY
			}
			log.Info("Succesfully Deleted "+m.Name+" Pod ", "POD.NAME", available[i-1].Name)

		}
	}

	readyPod, replicasFound, availableFinal, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		n.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	if readyPod.Name != "" {
		availableFinal = append(availableFinal, readyPod)
	}

	podNamesFinal := dbcommons.GetPodNames(availableFinal)
	log.Info(" ", "Final "+m.Name+" Pods After Deleting (or) Adding Extra Pods ( Including The Ready Pod ) ", podNamesFinal)
	log.Info(" ", m.Name+" Replicas Available : ", len(podNamesFinal))
	log.Info(" ", m.Name+" Replicas Required : ", replicasReq)

	return requeueN
}

//#############################################################################
//    Creating Oracle Wallet
//#############################################################################
func (r *StandbyDatabaseReconciler) createWallet(m *dbapi.StandbyDatabase, n *dbapi.SingleInstanceDatabase,
	ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// Listing all the pods
	readyPod, _, availableFinal, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		n.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, nil
	}
	if readyPod.Name != "" {
		return requeueN, nil
	}

	// Wallet is created in persistent volume, hence it only needs to be executed once for all number of pods
	if len(availableFinal) == 0 {
		r.Log.Info("Pods are being created, currently no pods available")
		return requeueY, nil
	}

	// Iterate through the avaialableFinal (list of pods) to find out the pod whose status is updated about the init containers
	// If no required pod found then requeue the reconcile request
	var pod corev1.Pod
	var podFound bool
	for _, pod = range availableFinal {
		// Check if pod status contianer is updated about init containers
		if len(pod.Status.InitContainerStatuses) > 0 {
			podFound = true
			break
		}
	}
	if !podFound {
		r.Log.Info("No pod has its status updated about init containers. Requeueing...")
		return requeueY, nil
	}

	lastInitContIndex := len(pod.Status.InitContainerStatuses) - 1

	// If InitContainerStatuses[<index_of_init_container>].Ready is true, it means that the init container is successful
	if pod.Status.InitContainerStatuses[lastInitContIndex].Ready {
		// Init container named "init-wallet" has completed it's execution, hence return and don't requeue
		return requeueN, nil
	}

	if pod.Status.InitContainerStatuses[lastInitContIndex].State.Running == nil {
		// Init container named "init-wallet" is not running, so waiting for it to come in running state requeueing the reconcile request
		r.Log.Info("Waiting for init-wallet to come in running state...")
		return requeueY, nil
	}

	r.Log.Info("Creating Wallet...")

	// Querying the secret
	r.Log.Info("Querying the database secret ...")
	secret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: m.Spec.AdminPassword.SecretName, Namespace: m.Namespace}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Log.Info("Secret not found")
			m.Status.Status = dbcommons.StatusError
			r.Status().Update(ctx, m)
			return requeueY, nil
		}
		r.Log.Error(err, "Unable to get the secret. Requeueing..")
		return requeueY, nil
	}

	// Execing into the pods and creating the wallet
	adminPassword := string(secret.Data[m.Spec.AdminPassword.SecretKey])

	out, err := dbcommons.ExecCommand(r, r.Config, pod.Name, pod.Namespace, "init-wallet",
		ctx, req, true, "bash", "-c", fmt.Sprintf("%s && %s && %s",
			dbcommons.WalletPwdCMD,
			dbcommons.WalletCreateCMD,
			fmt.Sprintf(dbcommons.WalletEntriesCMD, adminPassword)))
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, nil
	}
	r.Log.Info("Creating wallet entry Output : \n" + out)

	return requeueN, nil
}

//#############################################################################
//    Deleting the Oracle Wallet
//#############################################################################
func (r *StandbyDatabaseReconciler) deleteWallet(m *dbapi.StandbyDatabase, n *dbapi.SingleInstanceDatabase,
	ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// Deleting the secret and then deleting the wallet
	// If the secret is not found it means that the secret and wallet both are deleted, hence no need to requeue
	if !m.Spec.AdminPassword.KeepSecret {
		r.Log.Info("Querying the database secret ...")
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: m.Spec.AdminPassword.SecretName, Namespace: m.Namespace}, secret)
		if err == nil {
			err := r.Delete(ctx, secret)
			if err == nil {
				r.Log.Info("Deleted the secret : " + m.Spec.AdminPassword.SecretName)
			}
		}
	}

	// Getting the ready pod for the database
	readyPod, _, _, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		n.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, err
	}

	// Deleting the wallet
	_, err = dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "",
		ctx, req, false, "bash", "-c", dbcommons.WalletDeleteCMD)
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, nil
	}
	r.Log.Info("Wallet Deleted !!")
	return requeueN, nil
}

//#############################################################################
//    Update Reconcile Status
//#############################################################################
func (r *StandbyDatabaseReconciler) updateReconcileStatus(m *dbapi.StandbyDatabase, n *dbapi.SingleInstanceDatabase,
	readyPod corev1.Pod, ctx context.Context, req ctrl.Request) (err error) {

	if m.Status.Status != dbcommons.StatusReady {
		return
	}

	// DB is ready, fetch and update other info
	// ConnectStrings assigned in createSVC() , Ready assigned in validateDBReadiness()
	out, err := dbcommons.GetDatabaseRole(readyPod, r, r.Config, ctx, req, n.Spec.Edition)
	if err == nil {
		m.Status.Role = strings.ToUpper(out)
	}
	version, out, err := dbcommons.GetDatabaseVersion(readyPod, r, r.Config, ctx, req, n.Spec.Edition)
	if err == nil {
		m.Status.Version = version
	}
	return
}

//#############################################################################
//    Fetch the DB ready POD
//#############################################################################
func (r *StandbyDatabaseReconciler) validateDBReadiness(m *dbapi.StandbyDatabase,
	n *dbapi.SingleInstanceDatabase, sidbReadyPod corev1.Pod, ctx context.Context, req ctrl.Request) (ctrl.Result, corev1.Pod) {

	log := r.Log.WithValues("validateDBReadiness", req.NamespacedName)

	readyPod, _, available, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		n.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, readyPod
	}

	if readyPod.Name == "" {
		eventReason := "Database Pending"
		eventMsg := "Waiting for database pod to be ready"
		m.Status.Status = dbcommons.StatusPending
		if ok, _ := dbcommons.IsAnyPodWithStatus(available, corev1.PodFailed); ok {
			eventReason = "Database Failed"
			eventMsg = "Pod execution failed"
		} else if ok, runningPod := dbcommons.IsAnyPodWithStatus(available, corev1.PodRunning); ok {
			eventReason = "Database Creating"
			eventMsg = "Waiting for database to be ready"
			m.Status.Status = dbcommons.StatusCreating

			out, err := dbcommons.ExecCommand(r, r.Config, runningPod.Name, runningPod.Namespace, "",
				ctx, req, false, "bash", "-c", dbcommons.GetCheckpointFileCMD)
			if err != nil {
				r.Log.Error(err, err.Error())
				return requeueY, readyPod
			}
			r.Log.Info("GetCheckpointFileCMD Output : \n" + out)

			if out != "" {
				eventReason = "Database Unhealthy"
				eventMsg = "Datafiles exists"
				m.Status.Status = dbcommons.StatusNotReady
			}
		}
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
		r.Log.Info(eventMsg)
		// As No pod is ready now , turn on mode when pod is ready . so requeue the request
		return requeueY, readyPod
	}
	if m.Status.DatafilesCreated != "true" {
		// Change Tnsnames, Listener on Standby db after standby pod becomes ready
		result := r.setupNetworkConfiguration(m, n, sidbReadyPod, readyPod, ctx, req)
		if result.Requeue {
			return result, readyPod
		}
	}
	eventReason := "Database Ready"
	eventMsg := ""
	m.Status.DatafilesCreated = "true"
	r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

	return requeueN, readyPod

}

func (r *StandbyDatabaseReconciler) setupNetworkConfiguration(m *dbapi.StandbyDatabase,
	n *dbapi.SingleInstanceDatabase, sidbReadyPod corev1.Pod, standbyReadyPod corev1.Pod, ctx context.Context, req ctrl.Request) ctrl.Result {

	log := r.Log.WithValues("setupNetworkConfiguration", req.NamespacedName)

	// Get Pdbs on Primary Database
	out, err := dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "",
		ctx, req, false, "bash", "-c", fmt.Sprintf("echo -e  \"%s\"  | sqlplus -s / as sysdba", dbcommons.GetPdbsSQL))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	} else {
		log.Info("GetPdbsSQL Output")
		log.Info(out)
	}
	pdbs, err := dbcommons.StringToLines(out)

	for _, pdb := range pdbs {
		if pdb == "" {
			continue
		}
		// Get the Tnsnames.ora entries
		out, err = dbcommons.ExecCommand(r, r.Config, standbyReadyPod.Name, standbyReadyPod.Namespace, "",
			ctx, req, false, "bash", "-c", fmt.Sprintf("cat /opt/oracle/oradata/dbconfig/%s/tnsnames.ora", strings.ToUpper(m.Spec.Sid)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		} else {
			log.Info("tnsnames.ora Output")
			log.Info(out)
		}

		if strings.Contains(out, "(SERVICE_NAME = "+strings.ToUpper(pdb)+")") {
			log.Info("TNS ENTRY OF " + strings.ToUpper(pdb) + " ALREADY EXISTS ON SIDB ")
		} else {

			tnsnamesEntry := dbcommons.PDBTnsnamesEntry
			tnsnamesEntry = strings.ReplaceAll(tnsnamesEntry, "##PDB_NAME##", strings.ToUpper(pdb))

			// Add Tnsnames.ora For pdb on Standby Database
			out, err = dbcommons.ExecCommand(r, r.Config, standbyReadyPod.Name, standbyReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
				fmt.Sprintf("echo -e  \"%s\"  | cat >> /opt/oracle/oradata/dbconfig/%s/tnsnames.ora ", tnsnamesEntry, strings.ToUpper(m.Spec.Sid)))
			if err != nil {
				log.Error(err, err.Error())
				return requeueY
			} else {
				log.Info("Modifying tnsnames.ora for Pdb Output")
				log.Info(out)
			}

		}
	}

	// Add Tnsnames.ora of Primary_Sid on Standby Database
	out, err = dbcommons.ExecCommand(r, r.Config, standbyReadyPod.Name, standbyReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
		fmt.Sprintf("echo -e  \"%s\"  | cat >> /opt/oracle/oradata/dbconfig/%s/tnsnames.ora ", dbcommons.PrimaryTnsnamesEntry, strings.ToUpper(m.Spec.Sid)))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	} else {
		log.Info("Modifying tnsnames.ora Output")
		log.Info(out)
	}

	// Get Listener.ora on Standby Database
	out, err = dbcommons.ExecCommand(r, r.Config, standbyReadyPod.Name, standbyReadyPod.Namespace, "",
		ctx, req, false, "bash", "-c", fmt.Sprintf("cat /opt/oracle/oradata/dbconfig/%s/listener.ora ", strings.ToUpper(m.Spec.Sid)))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	} else {
		log.Info("listener.ora Output")
		log.Info(out)
	}

	if strings.Contains(out, strings.ToUpper(m.Spec.Sid)+"_DGMGRL") {
		log.Info("LISTENER.ORA ALREADY HAS " + strings.ToUpper(m.Spec.Sid) + "_DGMGRL ENTRY IN SID_LIST_LISTENER ")
	} else {

		// Modifying Listener.ora for DGMGRL entry on Standby Database
		out, err = dbcommons.ExecCommand(r, r.Config, standbyReadyPod.Name, standbyReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | cat > /opt/oracle/oradata/dbconfig/%s/listener.ora ", dbcommons.ListenerEntry, strings.ToUpper(m.Spec.Sid)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		} else {
			log.Info("Modifying listener.ora Output")
			log.Info(out)
		}

		// Stopping Listener on Standby Database
		out, err = dbcommons.ExecCommand(r, r.Config, standbyReadyPod.Name, standbyReadyPod.Namespace, "",
			ctx, req, false, "bash", "-c", "lsnrctl stop")
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		} else {
			log.Info("lsnrctl stop Output")
			log.Info(out)
		}

		// Start Listener on Standby Database
		out, err = dbcommons.ExecCommand(r, r.Config, standbyReadyPod.Name, standbyReadyPod.Namespace, "",
			ctx, req, false, "bash", "-c", "lsnrctl start")
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		} else {
			log.Info("lsnrctl start Output")
			log.Info(out)
		}

	}
	return requeueN
}

//#############################################################################
//   Manage Finalizer to cleanup before deletion of StandbyDatabase
//#############################################################################
func (r *StandbyDatabaseReconciler) manageStandbyDatabaseDeletion(req ctrl.Request, ctx context.Context, m *dbapi.StandbyDatabase) ctrl.Result {
	log := r.Log.WithValues("manageStandbyDatabaseDeletion", req.NamespacedName)

	// Check if the StandbyDatabase instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isStandbyDatabaseMarkedToBeDeleted := m.GetDeletionTimestamp() != nil
	if isStandbyDatabaseMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(m, standbyDatabaseFinalizer) {
			// Run finalization logic for standbyDatabaseFinalizer. If the
			// finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			if err := r.cleanupStandbyDatabase(req, ctx, m); err != nil {
				log.Error(err, err.Error())
				return requeueY
			}

			// Remove standbyDatabaseFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			controllerutil.RemoveFinalizer(m, standbyDatabaseFinalizer)
			err := r.Update(ctx, m)
			if err != nil {
				log.Error(err, err.Error())
				return requeueY
			}
		}
		return requeueY
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(m, standbyDatabaseFinalizer) {
		controllerutil.AddFinalizer(m, standbyDatabaseFinalizer)
		err := r.Update(ctx, m)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
	}
	return requeueN
}

//#############################################################################
//   Finalization logic for standbyDatabaseFinalizer
//#############################################################################
func (r *StandbyDatabaseReconciler) cleanupStandbyDatabase(req ctrl.Request, ctx context.Context, m *dbapi.StandbyDatabase) error {
	log := r.Log.WithValues("cleanupStandbyDatabase", req.NamespacedName)
	// Cleanup steps that the operator needs to do before the CR can be deleted.

	log.Info("Successfully cleaned up StandbyDatabase")
	return nil
}

//#############################################################################
//    SetupWithManager sets up the controller with the Manager
//#############################################################################
func (r *StandbyDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbapi.StandbyDatabase{}).
		Owns(&corev1.Pod{}). //Watch for deleted pods of standbyDatabase Owner
		WithEventFilter(dbcommons.ResourceEventHandler()).
		WithOptions(controller.Options{MaxConcurrentReconciles: 100}). //ReconcileHandler is never invoked concurrently with the same object.
		Complete(r)
}
