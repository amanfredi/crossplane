/*
Copyright 2019 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplaneio/crossplane-runtime/pkg/logging"
	"github.com/crossplaneio/crossplane-runtime/pkg/meta"
	"github.com/crossplaneio/crossplane/apis/stacks/v1alpha1"
	"github.com/crossplaneio/crossplane/pkg/stacks"
)

// Labels used to track ownership across namespaces and scopes.
const (
	labelParentGroup     = "core.crossplane.io/parent-group"
	labelParentVersion   = "core.crossplane.io/parent-version"
	labelParentKind      = "core.crossplane.io/parent-kind"
	labelParentNamespace = "core.crossplane.io/parent-namespace"
	labelParentName      = "core.crossplane.io/parent-name"
	labelParentUID       = "core.crossplane.io/parent-uid"
	labelNamespaceFmt    = "namespace.crossplane.io/%s"
)

var (
	jobBackoff                = int32(0)
	registryDirName           = ".registry"
	packageContentsVolumeName = "package-contents"
	labelNamespaceFmt         = "namespace.crossplane.io/%s"
)

// JobCompleter is an interface for handling job completion
type jobCompleter interface {
	handleJobCompletion(ctx context.Context, i v1alpha1.StackInstaller, job *batchv1.Job) error
}

// StackInstallJobCompleter is a concrete implementation of the jobCompleter interface
type stackInstallJobCompleter struct {
	client       client.Client
	podLogReader Reader
}

func createInstallJob(i v1alpha1.StackInstaller, executorInfo *stacks.ExecutorInfo) *batchv1.Job {
	ref := meta.AsOwner(meta.ReferenceTo(i, i.GroupVersionKind()))
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            i.GetName(),
			Namespace:       i.GetNamespace(),
			OwnerReferences: []metav1.OwnerReference{ref},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &jobBackoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					InitContainers: []corev1.Container{
						{
							Name:    "stack-package",
							Image:   i.Image(),
							Command: []string{"cp", "-R", registryDirName, "/ext-pkg/"},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      packageContentsVolumeName,
									MountPath: "/ext-pkg",
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "stack-executor",
							Image: executorInfo.Image,
							// "--debug" can be added to this list of Args to get debug output from the job,
							// but note that will be included in the stdout from the pod, which makes it
							// impossible to create the resources that the job unpacks.
							Args: []string{
								"stack",
								"unpack",
								fmt.Sprintf("--content-dir=%s", filepath.Join("/ext-pkg", registryDirName)),
								"--permission-scope=" + i.PermissionScope(),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      packageContentsVolumeName,
									MountPath: "/ext-pkg",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: packageContentsVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
}

func (jc *stackInstallJobCompleter) handleJobCompletion(ctx context.Context, i v1alpha1.StackInstaller, job *batchv1.Job) error {
	var stackRecord *v1alpha1.Stack

	// find the pod associated with the given job
	podName, err := jc.findPodNameForJob(ctx, job)
	if err != nil {
		return err
	}

	// read full output from job by retrieving the logs for the job's pod
	b, err := jc.readPodLogs(job.Namespace, podName)
	if err != nil {
		return err
	}

	// decode and process all resources from job output
	d := yaml.NewYAMLOrJSONDecoder(b, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := d.Decode(&obj); err != nil {
			if err == io.EOF {
				// we reached the end of the job output
				break
			}
			return errors.Wrapf(err, "failed to parse output from job %s", job.Name)
		}

		// process and create the object that we just decoded
		if err := jc.createJobOutputObject(ctx, obj, i, job); err != nil {
			return err
		}

		if isStackObject(obj) {
			// we just created the stack record, try to fetch it now so that it can be returned
			stackRecord = &v1alpha1.Stack{}
			n := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
			if err := jc.client.Get(ctx, n, stackRecord); err != nil {
				return errors.Wrapf(err, "failed to retrieve created stack record %s/%s from job %s", obj.GetNamespace(), obj.GetName(), job.Name)
			}
		}
	}

	if stackRecord == nil {
		return errors.Errorf("failed to find a stack record from job %s", job.Name)
	}

	// save a reference to the stack record in the status of the stack install
	i.SetStackRecord(&corev1.ObjectReference{
		APIVersion: stackRecord.APIVersion,
		Kind:       stackRecord.Kind,
		Name:       stackRecord.Name,
		Namespace:  stackRecord.Namespace,
		UID:        stackRecord.ObjectMeta.UID,
	})

	return nil
}

// findPodNameForJob finds the pod name associated with the given job.  Note that this functions
// assumes only a single pod will be associated with the job.
func (jc *stackInstallJobCompleter) findPodNameForJob(ctx context.Context, job *batchv1.Job) (string, error) {
	podList, err := jc.findPodsForJob(ctx, job)
	if err != nil {
		return "", err
	}

	if len(podList.Items) != 1 {
		return "", errors.Errorf("pod list for job %s should only have 1 item, actual: %d", job.Name, len(podList.Items))
	}

	return podList.Items[0].Name, nil
}

func (jc *stackInstallJobCompleter) findPodsForJob(ctx context.Context, job *batchv1.Job) (*corev1.PodList, error) {
	podList := &corev1.PodList{}
	labelSelector := client.MatchingLabels{
		"job-name": job.Name,
	}
	nsSelector := client.InNamespace(job.Namespace)
	if err := jc.client.List(ctx, podList, labelSelector, nsSelector); err != nil {
		return nil, err
	}

	return podList, nil
}

func (jc *stackInstallJobCompleter) readPodLogs(namespace, name string) (*bytes.Buffer, error) {
	podLogs, err := jc.podLogReader.GetReader(namespace, name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get logs request stream from pod %s", name)
	}
	defer func() { _ = podLogs.Close() }()

	b := new(bytes.Buffer)
	if _, err = io.Copy(b, podLogs); err != nil {
		return nil, errors.Wrapf(err, "failed to copy logs request stream from pod %s", name)
	}

	return b, nil
}

func generateNamespaceClusterRoles(i v1alpha1.StackInstaller) (roles []*rbacv1.ClusterRole) {
	personas := []string{"admin", "edit", "view"}

	namespaced := (i.PermissionScope() == string(apiextensions.NamespaceScoped))
	if !namespaced {
		return
	}

	ns := i.GetNamespace()
	for _, persona := range personas {
		name := fmt.Sprintf("crossplane:ns:%s:%s", ns, persona)
		role := &rbacv1.ClusterRole{
			TypeMeta: metav1.TypeMeta{
				Kind:       "ClusterRole",
				APIVersion: "rbac.authorization.k8s.io/v1",
			},
			AggregationRule: &rbacv1.AggregationRule{
				ClusterRoleSelectors: []metav1.LabelSelector{
					{
						MatchLabels: map[string]string{
							fmt.Sprintf("rbac.crossplane.io/aggregate-to-namespace-%s", persona): "true",
							fmt.Sprintf("namespace.crossplane.io/%s", ns):                        "true",
						},
					},
					{
						MatchLabels: map[string]string{
							fmt.Sprintf("rbac.crossplane.io/aggregate-to-namespace-default-%s", persona): "true",
						},
					},
				},
			},
			// TODO(displague) set parent labels?
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{},
			},
		}
		if namespaced {
			labelNamespace := fmt.Sprintf(labelNamespaceFmt, ns)

			role.ObjectMeta.Labels[labelNamespace] = "true"
		}
		roles = append(roles, role)
	}

	return roles
}

func (jc *stackInstallJobCompleter) createNamespaceClusterRoles(ctx context.Context, i v1alpha1.StackInstaller) error {
	roles := generateNamespaceClusterRoles(i)

	for _, role := range roles {
		if err := jc.client.Create(ctx, role); err != nil && !kerrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "failed to create clusterrole %s for stackinstall %s", role.GetName(), i.GetName())
		}
	}
	return nil
}

// createJobOutputObject names, labels, sets ownership, and creates resources
// resulting from a StackInstall or ClusterStackInstall. These expected
// resources are currently CRD and Stack objects
func (jc *stackInstallJobCompleter) createJobOutputObject(ctx context.Context, obj *unstructured.Unstructured,
	i v1alpha1.StackInstaller, job *batchv1.Job) error {

	// if we decoded a non-nil unstructured object, try to create it now
	if obj == nil {
		return nil
	}

	// when the current object is a Stack object, make sure the name and namespace are
	// set to match the current StackInstall (if they haven't already been set). Also,
	// set the owner reference of the Stack to be the StackInstall.
	if isStackObject(obj) {
		if obj.GetName() == "" {
			obj.SetName(i.GetName())
		}
		if obj.GetNamespace() == "" {
			obj.SetNamespace(i.GetNamespace())
		}

		obj.SetOwnerReferences([]metav1.OwnerReference{
			meta.AsOwner(meta.ReferenceTo(i, i.GroupVersionKind())),
		})
	}

	// We want to clean up any installed CRDS when we're deleted. We can't rely
	// on garbage collection because a namespaced object (StackInstall) can't
	// own a cluster scoped object (CustomResourceDefinition), so we use labels
	// instead.
	gvk := i.GroupVersionKind()
	labels := map[string]string{
		labelParentGroup:     gvk.Group,
		labelParentVersion:   gvk.Version,
		labelParentKind:      gvk.Kind,
		labelParentNamespace: i.GetNamespace(),
		labelParentName:      i.GetName(),
		labelParentUID:       string(i.GetUID()),
	}

	if isCRDObject(obj) {
		labelNamespaceFmt := "namespace.crossplane.io/%s"
		labelNamespace := fmt.Sprintf(labelNamespaceFmt, i.GetNamespace())

		labels[labelNamespace] = "true"

		if err := jc.createNamespaceClusterRoles(ctx, i); err != nil {
			return errors.Wrapf(err, "failed to create namespace persona clusterroles for stackinstall %s from job output %s", i.GetName(), job.GetName())
		}
	}

	meta.AddLabels(obj, labels)

	// TODO(displague) pass/inject a controller specific logger
	log.V(logging.Debug).Info(
		"creating object from job output",
		"job", job.Name,
		"name", obj.GetName(),
		"namespace", obj.GetNamespace(),
		"apiVersion", obj.GetAPIVersion(),
		"kind", obj.GetKind(),
		"parentGroup", labels[labelParentGroup],
		"parentVersion", labels[labelParentVersion],
		"parentKind", labels[labelParentKind],
		"parentName", labels[labelParentName],
		"parentNamespace", labels[labelParentNamespace],
		"parentUID", labels[labelParentUID],
	)
	if err := jc.client.Create(ctx, obj); err != nil && !kerrors.IsAlreadyExists(err) {
		return errors.Wrapf(err, "failed to create object %s from job output %s", obj.GetName(), job.Name)
	}

	return nil
}

func isStackObject(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}

	gvk := obj.GroupVersionKind()
	return gvk.Group == v1alpha1.Group && gvk.Version == v1alpha1.Version &&
		strings.EqualFold(gvk.Kind, v1alpha1.StackKind)
}

func isCRDObject(obj runtime.Object) bool {
	if obj == nil {
		return false
	}
	gvk := obj.GetObjectKind().GroupVersionKind()

	return apiextensions.SchemeGroupVersion == gvk.GroupVersion() &&
		strings.EqualFold(gvk.Kind, "CustomResourceDefinition")
}
