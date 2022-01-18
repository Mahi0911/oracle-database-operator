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

	dbapi "github.com/oracle/oracle-database-operator/apis/database/v1alpha1"
	dbcommons "github.com/oracle/oracle-database-operator/commons/database"
)

// DataguardBrokerReconciler reconciles a DataguardBroker object
type DataguardBrokerReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Config   *rest.Config
	Recorder record.EventRecorder
}

const dataguardBrokerFinalizer = "database.oracle.com/dataguardbrokerfinalizer"

//+kubebuilder:rbac:groups=database.oracle.com,resources=dataguardbrokers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=database.oracle.com,resources=dataguardbrokers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=database.oracle.com,resources=dataguardbrokers/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods;pods/log;pods/exec;persistentvolumeclaims;services;nodes;events,verbs=create;delete;get;list;patch;update;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DataguardBroker object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *DataguardBrokerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	r.Log.Info("Reconcile requested")

	dataguardBroker := &dbapi.DataguardBroker{}
	err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, dataguardBroker)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Log.Info("Resource deleted")
			return requeueN, nil
		}
		return requeueN, err
	}

	// Manage SingleInstanceDatabase Deletion
	result := r.manageDataguardBrokerDeletion(req, ctx, dataguardBroker)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Fetch Primary Database Reference
	singleInstanceDatabase := &dbapi.SingleInstanceDatabase{}
	err = r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: dataguardBroker.Spec.PrimaryDatabaseRef}, singleInstanceDatabase)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Log.Info("Resource deleted")
			return requeueN, nil
		}
		return requeueN, err
	}

	// Always refresh status before a reconcile
	defer r.Status().Update(ctx, dataguardBroker)

	// Create Service to point to primary database always
	result = r.createSVC(ctx, req, dataguardBroker)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Validate if Primary Database Reference is ready
	result, sidbReadyPod, adminPassword := r.validateSidbReadiness(dataguardBroker, singleInstanceDatabase, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Setup the DG Configuration
	result = r.setupDataguardBrokerConfiguration(dataguardBroker, singleInstanceDatabase, sidbReadyPod, adminPassword, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Set FSFO Targets according to FastStartFailover.Strategy
	result = r.setFSFOTargets(dataguardBroker, singleInstanceDatabase, sidbReadyPod, adminPassword, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Set a particular database as primary
	result = r.SetAsPrimaryDatabase(singleInstanceDatabase.Spec.Sid, dataguardBroker.Spec.SetAsPrimaryDatabase, dataguardBroker,
		singleInstanceDatabase, adminPassword, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Create PVC for Observer Pod
	result = r.createPVC(ctx, req, dataguardBroker, singleInstanceDatabase)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Create Observer Pod
	result = r.createPod(ctx, req, dataguardBroker, singleInstanceDatabase, sidbReadyPod, adminPassword)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Validate the Observer Readiness
	result = r.validateDGObserverReadiness(dataguardBroker, singleInstanceDatabase, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	r.Log.Info("Reconcile completed")
	return ctrl.Result{}, nil

}

//#####################################################################################################
//    Validate Readiness of the primary DB specified
//#####################################################################################################
func (r *DataguardBrokerReconciler) validateSidbReadiness(m *dbapi.DataguardBroker,
	n *dbapi.SingleInstanceDatabase, ctx context.Context, req ctrl.Request) (ctrl.Result, corev1.Pod, string) {

	log := r.Log.WithValues("validateSidbReadiness", req.NamespacedName)
	adminPassword := ""
	// ## FETCH THE SIDB REPLICAS .
	sidbReadyPod, _, _, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		n.Spec.Image.PullFrom, n.Name, n.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, sidbReadyPod, adminPassword
	}

	if n.Status.Status != dbcommons.StatusReady {

		eventReason := "Waiting"
		eventMsg := "Waiting for " + n.Name + " to be Ready"
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
		return requeueY, sidbReadyPod, adminPassword
	}

	// Validate databaseRef Admin Password
	adminPasswordSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: m.Spec.AdminPassword.SecretName, Namespace: m.Namespace}, adminPasswordSecret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			//m.Status.Status = dbcommons.StatusError
			eventReason := "Waiting"
			eventMsg := "waiting for secret : " + m.Spec.AdminPassword.SecretName + " to get created"
			r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
			r.Log.Info("Secret " + m.Spec.AdminPassword.SecretName + " Not Found")
			return requeueY, sidbReadyPod, adminPassword
		}
		log.Error(err, err.Error())
		return requeueY, sidbReadyPod, adminPassword
	}
	adminPassword = string(adminPasswordSecret.Data[m.Spec.AdminPassword.SecretKey])

	out, err := dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, true, "bash", "-c",
		fmt.Sprintf("echo -e  \"%s\"  | %s", fmt.Sprintf(dbcommons.ValidateAdminPassword, adminPassword), dbcommons.GetSqlClient(n.Spec.Edition)))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, sidbReadyPod, adminPassword
	}
	if strings.Contains(out, "USER is \"SYS\"") {
		log.Info("validated Admin password successfully")
	} else if strings.Contains(out, "ORA-01017") {
		//m.Status.Status = dbcommons.StatusError
		eventReason := "Logon denied"
		eventMsg := "invalid databaseRef admin password. secret: " + m.Spec.AdminPassword.SecretName
		r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, eventMsg)
		return requeueY, sidbReadyPod, adminPassword
	} else {
		return requeueY, sidbReadyPod, adminPassword
	}

	return requeueN, sidbReadyPod, adminPassword
}

//#############################################################################
//    Instantiate Service spec from StandbyDatabase spec
//#############################################################################
func (r *DataguardBrokerReconciler) instantiateSVCSpec(m *dbapi.DataguardBroker) *corev1.Service {
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
			Type: corev1.ServiceType(func() string {
				if m.Spec.LoadBalancer {
					return "LoadBalancer"
				}
				return "NodePort"
			}()),
		},
	}
	// Set StandbyDatabase instance as the owner and controller
	ctrl.SetControllerReference(m, svc, r.Scheme)
	return svc
}

//#############################################################################
//    Instantiate Persistent Volume Claim spec from StandbyDatabase spec
//#############################################################################
func (r *DataguardBrokerReconciler) instantiatePVCSpec(m *dbapi.DataguardBroker, n *dbapi.SingleInstanceDatabase) *corev1.PersistentVolumeClaim {

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
				accessMode = append(accessMode, corev1.PersistentVolumeAccessMode(n.Spec.Persistence.AccessMode))
				return accessMode
			}(),
			Resources: corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{
					// Requests describes the minimum amount of compute resources required
					"storage": resource.MustParse(n.Spec.Persistence.Size),
				},
			},
			StorageClassName: &n.Spec.Persistence.StorageClass,
		},
	}
	// Set StandbyDatabase instance as the owner and controller
	ctrl.SetControllerReference(m, pvc, r.Scheme)
	return pvc
}

//#############################################################################
//    Instantiate POD spec from DataguardBroker spec for Observer
//#############################################################################
func (r *DataguardBrokerReconciler) instantiatePodSpec(m *dbapi.DataguardBroker, n *dbapi.SingleInstanceDatabase) *corev1.Pod {

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
						Name:  "PRIMARY_DB_CONN_STR",
						Value: n.Name + ":1521/" + n.Spec.Sid,
					},
					{
						Name:  "DG_OBSERVER_ONLY",
						Value: "true",
					},
					{
						Name:  "DG_OBSERVER_NAME",
						Value: m.Name,
					},
					{
						// Sid used here only for Locking mechanism to work .
						Name:  "ORACLE_SID",
						Value: "OBSRVR" + strings.ToUpper(n.Spec.Sid),
					},
					{
						Name: "ORACLE_PWD",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: n.Spec.AdminPassword.SecretName,
								},
								Key: n.Spec.AdminPassword.SecretKey,
							},
						},
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

	// Set dataguardBroker instance as the owner and controller
	ctrl.SetControllerReference(m, pod, r.Scheme)
	return pod
}

//#############################################################################
//    Create a Service for StandbyDatabase
//#############################################################################
func (r *DataguardBrokerReconciler) createSVC(ctx context.Context, req ctrl.Request,
	m *dbapi.DataguardBroker) ctrl.Result {

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
		//err = r.Update(ctx, svc)
		if err != nil {
			log.Error(err, "Failed to create new Service", "Service.Namespace", svc.Namespace, "Service.Name", svc.Name)
			return requeueY
		} else {
			timeout := 30
			// Waiting for Service to get created as sometimes it takes some time to create a service . 30 seconds TImeout
			err = dbcommons.WaitForStatusChange(r, svc.Name, m.Namespace, ctx, req, time.Duration(timeout)*time.Second, "svc", "creation")
			if err != nil {
				log.Error(err, "Error in Waiting for svc status for Creation", "svc.Namespace", svc.Namespace, "SVC.Name", svc.Name)
				return requeueY
			}
			log.Info("Succesfully Created New Service ", "Service.Name : ", svc.Name)
		}

	} else if err != nil {
		log.Error(err, "Failed to get Service")
		return requeueY
	} else if err == nil {
		log.Info(" ", "Found Existing Service ", svc.Name)
	}

	return requeueN
}

//#############################################################################
//    Stake a claim for Persistent Volume
//#############################################################################
func (r *DataguardBrokerReconciler) createPVC(ctx context.Context, req ctrl.Request,
	m *dbapi.DataguardBroker, n *dbapi.SingleInstanceDatabase) ctrl.Result {

	log := r.Log.WithValues("createPVC", req.NamespacedName)
	if !m.Spec.FastStartFailOver.Enable {
		log.Info("FastStartFailOver is Not Enabled")
		return requeueN
	}

	// Check if the PVC already exists using r.Get, if not create a new one using r.Create
	foundpvc := &corev1.PersistentVolumeClaim{}
	// Get retrieves an obj for the given object key from the Kubernetes Cluster.
	// obj must be a struct pointer so that obj can be updated with the response returned by the Server.
	// Here foundpvc is the struct pointer to corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, foundpvc)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new PVC
		pvc := r.instantiatePVCSpec(m, n)
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
	log.Info("", "Found Existing PVC ", foundpvc.Name)

	return requeueN
}

//#############################################################################
//    Create the DataguardBroker POD for Observer
//#############################################################################
func (r *DataguardBrokerReconciler) createPod(ctx context.Context, req ctrl.Request, m *dbapi.DataguardBroker,
	n *dbapi.SingleInstanceDatabase, sidbReadyPod corev1.Pod, adminPassword string) ctrl.Result {

	log := r.Log.WithValues("createPOD", req.NamespacedName)
	if !m.Spec.FastStartFailOver.Enable {
		log.Info("FastStartFailOver is Not Enabled")
		return requeueN
	}

	// ## FETCH THE DATAGUARD REPLICAS .
	_, dataguardBrokerReplicasFound, _, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		n.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}

	if dataguardBrokerReplicasFound > 0 {
		return requeueN
	}

	_, _, err = dbcommons.GetDatabasesInDgConfig(sidbReadyPod, r, r.Config, ctx, req)
	if err != nil {
		eventReason := "Waiting"
		eventMsg := "Waiting for Standby DB to Create DG configuration"
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

		log.Info("DG CONFIGURATION DOES NOT EXIST")
		log.Error(err, err.Error())
		// As DG configuration doesnt exist (no standby has been created ) . so requeue the request
		return requeueY
	}

	// ## CHECK IF DG CONFIGURATION AVAILABLE ##
	out, err := dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
		fmt.Sprintf("echo -e  \"%s\"  | dgmgrl sys/%s@${ORACLE_SID} ", dbcommons.DBShowConfigCMD, adminPassword))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	log.Info("ShowConfiguration Output")
	log.Info(out)

	if !strings.Contains(out, "Fast-Start Failover: Enabled") {
		// ## ENABLE FSFO
		out1, err := dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | dgmgrl sys/%s@${ORACLE_SID} ", dbcommons.EnableFSFOCMD, adminPassword))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
		log.Info("EnableFastStartFailover Output")
		log.Info(out1)

	}
	if strings.Contains(out, "ORA-16532") || strings.Contains(out, "ORA-16525") || strings.Contains(out, "ORA-12514") {
		eventReason := "Waiting"
		eventMsg := "Waiting for Standby DB to Create DG configuration"
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

		log.Info("DG CONFIGURATION DOES NOT EXIST")
		// As DG configuration doesnt exist (no Standby has been created ) . so requeue the request
		return requeueY
	}

	pod := r.instantiatePodSpec(m, n)
	log.Info("Creating a new  POD", "POD.Namespace", pod.Namespace, "POD.Name", pod.Name)
	err = r.Create(ctx, pod)
	if err != nil {
		log.Error(err, "Failed to create new POD", "pod.Namespace", pod.Namespace, "POD.Name", pod.Name)
		return requeueY
	}
	timeout := 30
	// Waiting for Pod to get created as sometimes it takes some time to create a Pod . 30 seconds TImeout
	err = dbcommons.WaitForStatusChange(r, pod.Name, m.Namespace, ctx, req, time.Duration(timeout)*time.Second, "pod", "creation")
	if err != nil {
		log.Error(err, "Error in Waiting for Pod status for Creation", "pod.Namespace", pod.Namespace, "POD.Name", pod.Name)
		return requeueY
	}
	log.Info("Succesfully Created New Pod ", "POD.NAME : ", pod.Name)

	eventReason := "SUCCESS"
	eventMsg := ""
	r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

	return requeueN
}

//#############################################################################
//    Setup the requested DG Configuration
//#############################################################################
func (r *DataguardBrokerReconciler) setupDataguardBrokerConfiguration(m *dbapi.DataguardBroker, n *dbapi.SingleInstanceDatabase,
	sidbReadyPod corev1.Pod, adminPassword string, ctx context.Context, req ctrl.Request) ctrl.Result {
	log := r.Log.WithValues("setupDataguardBrokerConfiguration", req.NamespacedName)

	for i := 0; i < len(m.Spec.StandbyDatabaseRefs); i++ {

		standbyDatabase := &dbapi.StandbyDatabase{}
		err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: m.Spec.StandbyDatabaseRefs[i]}, standbyDatabase)
		if err != nil {
			if apierrors.IsNotFound(err) {
				eventReason := "Warning"
				eventMsg := m.Spec.StandbyDatabaseRefs[i] + "not found"
				r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
				continue
			}
			log.Error(err, err.Error())
			return requeueY
		}

		// ## FETCH THE STANDBY REPLICAS .
		standbyDatabaseReadyPod, _, _, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
			n.Spec.Image.PullFrom, standbyDatabase.Name, standbyDatabase.Namespace, ctx, req)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}

		if standbyDatabase.Status.Status != dbcommons.StatusReady {

			eventReason := "Waiting"
			eventMsg := "Waiting for " + standbyDatabase.Name + " to be Ready"
			r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
			return requeueY

		}

		result := r.setupDataguardBrokerConfigurationForGivenDB(m, standbyDatabase, standbyDatabaseReadyPod, sidbReadyPod, ctx, req, adminPassword)
		if result.Requeue {
			return result
		}

		// Update Databases
		r.updateReconcileStatus(m, sidbReadyPod, ctx, req)
	}

	eventReason := "DG Configuration up to date"
	eventMsg := ""

	// Patch DataguardBroker Service to point selector to Current Primary Name
	result := r.patchService(m, sidbReadyPod, n, ctx, req)
	if result.Requeue {
		return result
	}

	r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

	return requeueN
}

//#############################################################################
//    Patch DataguardBroker Service to point selector to Current Primary Name
//#############################################################################
func (r *DataguardBrokerReconciler) patchService(m *dbapi.DataguardBroker, sidbReadyPod corev1.Pod, n *dbapi.SingleInstanceDatabase,
	ctx context.Context, req ctrl.Request) ctrl.Result {
	log := r.Log.WithValues("patchService", req.NamespacedName)
	databases, out, err := dbcommons.GetDatabasesInDgConfig(sidbReadyPod, r, r.Config, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	if !strings.Contains(out, "ORA-") {
		primarySid := strings.ToUpper(dbcommons.GetPrimaryDatabase(databases))
		primaryName := n.Name
		if primarySid != n.Spec.Sid {
			primaryName = n.Status.StandbyDatabases[primarySid]
		}

		// Patch DataguardBroker Service to point selector to Current Primary Name
		svc := &corev1.Service{}
		err = r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, svc)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
		svc.Spec.Selector["app"] = primaryName
		err = r.Update(ctx, svc)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}

		if m.Spec.LoadBalancer {
			if len(svc.Status.LoadBalancer.Ingress) > 0 {
				m.Status.ExternalConnectString = svc.Status.LoadBalancer.Ingress[0].IP + ":" + fmt.Sprint(svc.Spec.Ports[0].Port) + "/DATAGUARD"
			}
			m.Status.ClusterConnectString = svc.Name + "." + svc.Namespace + ":" + fmt.Sprint(svc.Spec.Ports[0].Port) + "/DATAGUARD"
		} else {
			nodeip := dbcommons.GetNodeIp(r, ctx, req)
			m.Status.ExternalConnectString = nodeip + ":" + fmt.Sprint(svc.Spec.Ports[0].NodePort) + "/DATAGUARD"
			m.Status.ClusterConnectString = svc.Name + "." + svc.Namespace + ":" + fmt.Sprint(svc.Spec.Ports[0].Port) + "/DATAGUARD"
		}
	}
	return requeueN
}

//#############################################################################
//    Set up DG Configuration for a given StandbyDatabase
//#############################################################################
func (r *DataguardBrokerReconciler) setupDataguardBrokerConfigurationForGivenDB(m *dbapi.DataguardBroker, standbyDatabase *dbapi.StandbyDatabase,
	standbyDatabaseReadyPod corev1.Pod, sidbReadyPod corev1.Pod, ctx context.Context, req ctrl.Request, adminPassword string) ctrl.Result {

	log := r.Log.WithValues("setupDataguardBrokerConfigurationForGivenDB", req.NamespacedName)

	if standbyDatabaseReadyPod.Name == "" || sidbReadyPod.Name == "" {
		return requeueY
	}

	// ## CHECK IF DG CONFIGURATION AVAILABLE ##
	out, err := dbcommons.ExecCommand(r, r.Config, standbyDatabaseReadyPod.Name, standbyDatabaseReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
		fmt.Sprintf("echo -e  \"%s\"  | dgmgrl sys/%s@${PRIMARY_DB_CONN_STR} ", dbcommons.DBShowConfigCMD, adminPassword))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	log.Info("ShowConfiguration Output")
	log.Info(out)

	if strings.Contains(out, "ORA-16525") {
		log.Info("ORA-16525: The Oracle Data Guard broker is not yet available on Primary")
		return requeueY
	}
	if strings.Contains(out, "ORA-16532") {
		//  ORA-16532: Oracle Data Guard broker configuration does not exist , so create one
		if m.Spec.ProtectionMode == "MaxPerformance" {
			// ## DG CONFIGURATION FOR PRIMARY DB || MODE : MAXPERFORMANCE ##
			out, err := dbcommons.ExecCommand(r, r.Config, standbyDatabaseReadyPod.Name, standbyDatabaseReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
				fmt.Sprintf("echo -e  \"%s\"  | dgmgrl sys/%s@${PRIMARY_DB_CONN_STR} ", dbcommons.DataguardBrokerMaxPerformanceCMD, adminPassword))
			if err != nil {
				log.Error(err, err.Error())
				return requeueY
			}
			log.Info("DgConfigurationMaxPerformance Output")
			log.Info(out)

		} else if m.Spec.ProtectionMode == "MaxAvailability" {
			// ## DG CONFIGURATION FOR PRIMARY DB || MODE : MAX AVAILABILITY ##
			out, err = dbcommons.ExecCommand(r, r.Config, standbyDatabaseReadyPod.Name, standbyDatabaseReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
				fmt.Sprintf("echo -e  \"%s\"  | dgmgrl sys/%s@${PRIMARY_DB_CONN_STR} ", dbcommons.DataguardBrokerMaxAvailabilityCMD, adminPassword))
			if err != nil {
				log.Error(err, err.Error())
				return requeueY
			}
			log.Info("DgConfigurationMaxAvailability Output")
			log.Info(out)

		} else {
			log.Info("SPECIFY correct Protection Mode . Either MaxAvailability or MaxPerformance")
			return requeueY
		}

		// ## SHOW CONFIGURATION DG
		out, err := dbcommons.ExecCommand(r, r.Config, standbyDatabaseReadyPod.Name, standbyDatabaseReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | dgmgrl sys/%s@${PRIMARY_DB_CONN_STR} ", dbcommons.DBShowConfigCMD, adminPassword))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		} else {
			log.Info("ShowConfiguration Output")
			log.Info(out)
		}
		return requeueN
	}

	// DG Configuration Exists . So add the standbyDatabase to the existing DG Configuration
	databases, out, err := dbcommons.GetDatabasesInDgConfig(sidbReadyPod, r, r.Config, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}

	// ## ADD DATABASE TO DG CONFIG , IF NOT PRESENT
	found, _ := dbcommons.IsDatabaseFound(standbyDatabase.Spec.Sid, databases, "")
	if found {
		return requeueN
	}
	primary := dbcommons.GetPrimaryDatabase(databases)
	if m.Spec.ProtectionMode == "MaxPerformance" {
		// ## DG CONFIGURATION FOR PRIMARY DB || MODE : MAXPERFORMANCE ##
		out, err := dbcommons.ExecCommand(r, r.Config, standbyDatabaseReadyPod.Name, standbyDatabaseReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | dgmgrl sys/%s@%s ", dbcommons.DataguardBrokerAddDBMaxPerformanceCMD, adminPassword, primary))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
		log.Info("DgConfigurationMaxPerformance Output")
		log.Info(out)

	} else if m.Spec.ProtectionMode == "MaxAvailability" {
		// ## DG CONFIGURATION FOR PRIMARY DB || MODE : MAX AVAILABILITY ##
		out, err = dbcommons.ExecCommand(r, r.Config, standbyDatabaseReadyPod.Name, standbyDatabaseReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | dgmgrl sys/%s@%s ", dbcommons.DataguardBrokerAddDBMaxAvailabilityCMD, adminPassword, primary))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
		log.Info("DgConfigurationMaxAvailability Output")
		log.Info(out)

	} else {
		log.Info("SPECIFY correct Protection Mode . Either MaxAvailability or MaxPerformance")
		log.Error(err, err.Error())
		return requeueY
	}

	databases, out, err = dbcommons.GetDatabasesInDgConfig(sidbReadyPod, r, r.Config, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}

	// ## SET PROPERTY FASTSTARTFAILOVERTARGET FOR EACH DATABASE TO ALL OTHER DATABASES IN DG CONFIG .
	for i := 0; i < len(databases); i++ {
		out, err = dbcommons.ExecCommand(r, r.Config, standbyDatabaseReadyPod.Name, standbyDatabaseReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"EDIT DATABASE %s SET PROPERTY FASTSTARTFAILOVERTARGET=%s \"  | dgmgrl sys/%s@%s ",
				strings.Split(databases[i], ":")[0], getFSFOTargets(i, databases), adminPassword, primary))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
		log.Info("SETTING FSFO TARGET OUTPUT")
		log.Info(out)

		out, err = dbcommons.ExecCommand(r, r.Config, standbyDatabaseReadyPod.Name, standbyDatabaseReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"SHOW DATABASE %s FASTSTARTFAILOVERTARGET  \"  | dgmgrl sys/%s@%s ",
				strings.Split(databases[i], ":")[0], adminPassword, primary))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
		log.Info("FSFO TARGETS OF " + databases[i])
		log.Info(out)

	}

	return requeueN
}

//#############################################################################
//    Return FSFO targets of each StandbyDatabase
//    Concatenation of all strings in databases slice expecting that of index 1
//#############################################################################
func getFSFOTargets(index int, databases []string) string {
	fsfotargets := ""
	for i := 0; i < len(databases); i++ {
		if i != index {
			splitstr := strings.Split(databases[i], ":")
			if fsfotargets == "" {
				fsfotargets = splitstr[0]
			} else {
				fsfotargets = fsfotargets + "," + splitstr[0]
			}
		}
	}
	return fsfotargets
}

//#####################################################################################################
//                           SET FSFO TARGETS ACCORDINGLY TO FSFOCONFIG
//#####################################################################################################
func (r *DataguardBrokerReconciler) setFSFOTargets(m *dbapi.DataguardBroker, n *dbapi.SingleInstanceDatabase,
	sidbReadyPod corev1.Pod, adminPassword string, ctx context.Context, req ctrl.Request) ctrl.Result {

	log := r.Log.WithValues("setFSFOTargets", req.NamespacedName)

	if sidbReadyPod.Name == "" {
		return requeueY
	}
	databases, _, err := dbcommons.GetDatabasesInDgConfig(sidbReadyPod, r, r.Config, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	if len(databases) < 2 {
		return requeueN
	}
	// Set FSFO Targets according to the input yaml of dataguardBroker
	for i, strategy := range m.Spec.FastStartFailOver.Strategy {
		found, _ := dbcommons.IsDatabaseFound(strategy.SourceDatabaseRef, databases, "")
		if found {
			splitstr := strings.Split(strategy.TargetDatabaseRefs, ",")
			for j := 0; j < len(splitstr); j++ {
				found, _ := dbcommons.IsDatabaseFound(splitstr[j], databases, "")
				if !found {
					log.Info("Waiting for " + splitstr[j] + " to get added in dg configuration")
					return requeueY
				}
			}

		} else {
			log.Info("Waiting for " + strategy.SourceDatabaseRef + " to get added in dg configuration")
			return requeueY
		}
		primary := dbcommons.GetPrimaryDatabase(databases)
		out, err := dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"EDIT DATABASE %s SET PROPERTY FASTSTARTFAILOVERTARGET=%s \"  | dgmgrl sys/%s@%s ",
				strategy.SourceDatabaseRef, adminPassword, strategy.TargetDatabaseRefs, primary))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
		log.Info("SETTING FSFO TARGET OUTPUT")
		log.Info(out)

		out, err = dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"SHOW DATABASE %s FASTSTARTFAILOVERTARGET  \"  | dgmgrl sys/%s@%s ", strategy.SourceDatabaseRef, adminPassword, primary))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
		log.Info("FSFO TARGETS OF " + databases[i])
		log.Info(out)
	}

	return requeueN
}

//#####################################################################################################
//  Switchovers to 'sid' db to make 'sid' db primary
//#####################################################################################################
func (r *DataguardBrokerReconciler) SetAsPrimaryDatabase(sidbSid string, targetSid string, m *dbapi.DataguardBroker, n *dbapi.SingleInstanceDatabase,
	adminPassword string, ctx context.Context, req ctrl.Request) ctrl.Result {

	log := r.Log.WithValues("SetAsPrimaryDatabase", req.NamespacedName)
	if targetSid == "" {
		log.Info("Specified sid is nil")
		return requeueN
	}

	// Fetch the SIDB Ready Pod
	sidbReadyPod, _, _, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		(n.Spec.Image.PullFrom), n.Name, n.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}

	// Fetch databases in dataguard broker configuration
	databases, _, err := dbcommons.GetDatabasesInDgConfig(sidbReadyPod, r, r.Config, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}

	// Fetch the current Primary database
	primarySid := dbcommons.GetPrimaryDatabase(databases)
	if strings.EqualFold(primarySid, targetSid) {
		log.Info(targetSid + " is already Primary")
		return requeueN
	}
	found, _ := dbcommons.IsDatabaseFound(targetSid, databases, "")
	if !found {
		log.Info(targetSid + " not yet set in DG config")
		return requeueY
	}

	// Fetch the PrimarySid Ready Pod to create chk file
	var primaryReq ctrl.Request
	var primaryReadyPod corev1.Pod
	if !strings.EqualFold(primarySid, sidbSid) {
		primaryReq = ctrl.Request{
			NamespacedName: types.NamespacedName{
				Namespace: req.Namespace,
				Name:      n.Status.StandbyDatabases[strings.ToUpper(primarySid)],
			},
		}
		primaryReadyPod, _, _, _, err = dbcommons.FindPods(r, "", "", primaryReq.Name, primaryReq.Namespace, ctx, req)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
	} else {
		primaryReadyPod = sidbReadyPod
	}

	// Fetch the targetSid Ready Pod to create chk file
	var targetReq ctrl.Request
	var targetReadyPod corev1.Pod
	if !strings.EqualFold(targetSid, sidbSid) {
		targetReq = ctrl.Request{
			NamespacedName: types.NamespacedName{
				Namespace: req.Namespace,
				Name:      n.Status.StandbyDatabases[strings.ToUpper(targetSid)],
			},
		}
		targetReadyPod, _, _, _, err = dbcommons.FindPods(r, "", "", targetReq.Name, targetReq.Namespace, ctx, req)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
	} else {
		targetReadyPod = sidbReadyPod
	}

	// Create a chk File so that no other pods take the lock during Switchover .
	out, err := dbcommons.ExecCommand(r, r.Config, primaryReadyPod.Name, primaryReadyPod.Namespace, "", ctx, req, false, "bash", "-c", dbcommons.CreateChkFileCMD)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	out, err = dbcommons.ExecCommand(r, r.Config, targetReadyPod.Name, targetReadyPod.Namespace, "", ctx, req, false, "bash", "-c", dbcommons.CreateChkFileCMD)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	log.Info("Successfully Created chk file " + out)

	eventReason := "Waiting"
	eventMsg := "Switchover In Progress"
	r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

	// Connect to 'primarySid' db using dgmgrl and switchover to 'targetSid' db to make 'targetSid' db primary
	out, err = dbcommons.ExecCommand(r, r.Config, sidbReadyPod.Name, sidbReadyPod.Namespace, "", ctx, req, false, "bash", "-c",
		fmt.Sprintf("echo -e  \"SWITCHOVER TO %s\"  | dgmgrl sys/%s@%s ", targetSid, adminPassword, primarySid))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	log.Info("SWITCHOVER TO " + targetSid + " Output")
	log.Info(out)

	eventReason = "Success"
	eventMsg = "Switchover Completed Successfully"
	r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

	// Remove the chk File .
	out, err = dbcommons.ExecCommand(r, r.Config, primaryReadyPod.Name, primaryReadyPod.Namespace, "", ctx, req, false, "bash", "-c", dbcommons.RemoveChkFileCMD)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	out, err = dbcommons.ExecCommand(r, r.Config, targetReadyPod.Name, targetReadyPod.Namespace, "", ctx, req, false, "bash", "-c", dbcommons.RemoveChkFileCMD)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}
	log.Info("Successfully Removed chk file " + out)

	// Update Databases
	r.updateReconcileStatus(m, sidbReadyPod, ctx, req)

	// Update status of Primary true/false on 'primary' db (From which switchover initiated)
	if !strings.EqualFold(primarySid, sidbSid) {

		standbyDatabase := &dbapi.StandbyDatabase{}
		err = r.Get(ctx, primaryReq.NamespacedName, standbyDatabase)
		if err != nil {
			return requeueN
		}
		out, err := dbcommons.GetDatabaseRole(primaryReadyPod, r, r.Config, ctx, primaryReq, n.Spec.Edition)
		if err == nil {
			standbyDatabase.Status.Role = strings.ToUpper(out)
		}
		r.Status().Update(ctx, standbyDatabase)

	} else {
		sidbReq := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Namespace: req.Namespace,
				Name:      n.Name,
			},
		}
		out, err := dbcommons.GetDatabaseRole(sidbReadyPod, r, r.Config, ctx, sidbReq, n.Spec.Edition)
		if err == nil {
			n.Status.Role = strings.ToUpper(out)
		}
		r.Status().Update(ctx, n)
	}

	// Update status of Primary true/false on 'sid' db (To which switchover initiated)
	if !strings.EqualFold(targetSid, sidbSid) {

		standbyDatabase := &dbapi.StandbyDatabase{}
		err = r.Get(ctx, targetReq.NamespacedName, standbyDatabase)
		if err != nil {
			return requeueN
		}
		out, err := dbcommons.GetDatabaseRole(targetReadyPod, r, r.Config, ctx, targetReq, n.Spec.Edition)
		if err == nil {
			standbyDatabase.Status.Role = strings.ToUpper(out)
		}
		r.Status().Update(ctx, standbyDatabase)

	} else {
		sidbReq := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Namespace: req.Namespace,
				Name:      n.Name,
			},
		}
		out, err := dbcommons.GetDatabaseRole(sidbReadyPod, r, r.Config, ctx, sidbReq, n.Spec.Edition)
		if err == nil {
			n.Status.Role = strings.ToUpper(out)
		}
		r.Status().Update(ctx, n)
	}

	// Patch DataguardBroker Service to point selector to Current Primary Name and updates client db connection strings on dataguardBroker
	result := r.patchService(m, sidbReadyPod, n, ctx, req)
	if result.Requeue {
		return result
	}

	return requeueN
}

//#############################################################################
//    Update Reconcile Status
//#############################################################################
func (r *DataguardBrokerReconciler) updateReconcileStatus(m *dbapi.DataguardBroker, sidbReadyPod corev1.Pod,
	ctx context.Context, req ctrl.Request) (err error) {

	// ConnectStrings updated in PatchService()
	var databases []string
	databases, _, err = dbcommons.GetDatabasesInDgConfig(sidbReadyPod, r, r.Config, ctx, req)
	if err == nil {
		primaryDatabase := ""
		standbyDatabases := ""
		for i := 0; i < len(databases); i++ {
			splitstr := strings.Split(databases[i], ":")
			if strings.ToUpper(splitstr[1]) == "PRIMARY" {
				primaryDatabase = strings.ToUpper(splitstr[0])
			}
			if strings.ToUpper(splitstr[1]) == "PHYSICAL_STANDBY" {
				if standbyDatabases != "" {
					standbyDatabases += "," + strings.ToUpper(splitstr[0])
				} else {
					standbyDatabases = strings.ToUpper(splitstr[0])
				}
			}
		}
		m.Status.PrimaryDatabase = primaryDatabase
		m.Status.StandbyDatabases = standbyDatabases
	}
	return
}

//#############################################################################
//    Fetch the DB ready POD
//#############################################################################
func (r *DataguardBrokerReconciler) validateDGObserverReadiness(m *dbapi.DataguardBroker,
	n *dbapi.SingleInstanceDatabase, ctx context.Context, req ctrl.Request) ctrl.Result {

	log := r.Log.WithValues("validateDGObserverReadiness", req.NamespacedName)
	if !m.Spec.FastStartFailOver.Enable {
		log.Info("FastStartFailOver is Not Enabled")
		return requeueN
	}

	readyPod, _, available, _, err := dbcommons.FindPods(r, n.Spec.Image.Version,
		n.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY
	}

	if readyPod.Name == "" {
		available = append(available, readyPod)
		eventReason := ""
		eventMsg := "Waiting for Observer to be ready"
		if ok, _ := dbcommons.IsAnyPodWithStatus(available, corev1.PodFailed); ok {
			eventReason = "Observer Failed"
		} else if ok, _ := dbcommons.IsAnyPodWithStatus(available, corev1.PodRunning); ok {
			eventReason = "Observer Initialised"
		} else if ok, _ := dbcommons.IsAnyPodWithStatus(available, corev1.PodPending); ok {
			eventReason = "Observer Pending"
		}
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
		return requeueY
	}

	eventReason := "Observer Ready"
	eventMsg := ""
	r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

	return requeueN

}

//#############################################################################
//   Manage Finalizer to cleanup before deletion of DataguardBroker
//#############################################################################
func (r *DataguardBrokerReconciler) manageDataguardBrokerDeletion(req ctrl.Request, ctx context.Context, m *dbapi.DataguardBroker) ctrl.Result {
	log := r.Log.WithValues("manageDataguardBrokerDeletion", req.NamespacedName)

	// Check if the DataguardBroker instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isDataguardBrokerMarkedToBeDeleted := m.GetDeletionTimestamp() != nil
	if isDataguardBrokerMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(m, dataguardBrokerFinalizer) {
			// Run finalization logic for dataguardBrokerFinalizer. If the
			// finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			if err := r.cleanupDataguardBroker(req, ctx, m); err != nil {
				log.Error(err, err.Error())
				return requeueY
			}

			// Remove dataguardBrokerFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			controllerutil.RemoveFinalizer(m, dataguardBrokerFinalizer)
			err := r.Update(ctx, m)
			if err != nil {
				log.Error(err, err.Error())
				return requeueY
			}
		}
		return requeueY
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(m, dataguardBrokerFinalizer) {
		controllerutil.AddFinalizer(m, dataguardBrokerFinalizer)
		err := r.Update(ctx, m)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY
		}
	}
	return requeueN
}

//#############################################################################
//   Finalization logic for DataguardBrokerFinalizer
//#############################################################################
func (r *DataguardBrokerReconciler) cleanupDataguardBroker(req ctrl.Request, ctx context.Context, m *dbapi.DataguardBroker) error {
	log := r.Log.WithValues("cleanupDataguardBroker", req.NamespacedName)
	// Cleanup steps that the operator needs to do before the CR can be deleted.
	log.Info("Successfully cleaned up Dataguard Broker")
	return nil
}

//#############################################################################
//    SetupWithManager sets up the controller with the Manager
//#############################################################################
func (r *DataguardBrokerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbapi.DataguardBroker{}).
		Owns(&corev1.Pod{}). //Watch for deleted pods of DataguardBroker Owner
		WithEventFilter(dbcommons.ResourceEventHandler()).
		WithOptions(controller.Options{MaxConcurrentReconciles: 100}). //ReconcileHandler is never invoked concurrently with the same object.
		Complete(r)
}
