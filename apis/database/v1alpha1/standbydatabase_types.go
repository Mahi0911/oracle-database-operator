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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// StandbyDatabaseSpec defines the desired state of StandbyDatabase
type StandbyDatabaseSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Sid                string                  `json:"sid"`
	PrimaryDatabaseRef string                  `json:"primaryDatabaseRef"`
	AdminPassword      StandbyDatabasePassword `json:"adminPassword"`

	// +k8s:openapi-gen=true
	// +kubebuilder:validation:Minimum=1
	Replicas     int                        `json:"replicas"`
	Persistence  StandbyDatabasePersistence `json:"persistence"`
	NodeSelector map[string]string          `json:"nodeSelector,omitempty"`
}

// StandbyDatabasePersistence defines the storage releated params
type StandbyDatabasePersistence struct {
	Size         string `json:"size"`
	StorageClass string `json:"storageClass"`

	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany
	AccessMode string `json:"accessMode"`
}

// StandbyDatabasePassword defines the secret containing Password mapped to secretKey
type StandbyDatabasePassword struct {
	SecretName string `json:"secretName,omitempty"`
	SecretKey  string `json:"secretKey"`
	KeepSecret bool   `json:"keepSecret"`
}

// StandbyDatabaseStatus defines the observed state of StandbyDatabase
type StandbyDatabaseStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Role                  string `json:"role,omitempty"`
	Status                string `json:"status,omitempty"`
	Version               string `json:"version,omitempty"`
	ExternalConnectString string `json:"externalConnectString,omitempty"`
	ClusterConnectString  string `json:"clusterConnectString,omitempty"`
	DatafilesCreated      string `json:"datafilesCreated,omitempty"`
	DgBrokerConfigured    bool   `json:"dgBrokerConfigured,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
// +kubebuilder:printcolumn:JSONPath=".status.status",name="Status",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.version",name="Version",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.role",name="Role",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.clusterConnectString",name="Cluster Connect Str",type="string",priority=1
// +kubebuilder:printcolumn:JSONPath=".status.externalConnectString",name="Connect Str",type="string"
// +kubebuilder:printcolumn:JSONPath=".spec.primaryDatabaseRef",name="Primary Database",type="string"

// StandbyDatabase is the Schema for the standbydatabases API
type StandbyDatabase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StandbyDatabaseSpec   `json:"spec,omitempty"`
	Status StandbyDatabaseStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// StandbyDatabaseList contains a list of StandbyDatabase
type StandbyDatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StandbyDatabase `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StandbyDatabase{}, &StandbyDatabaseList{})
}
