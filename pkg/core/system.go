/*
 * Copyright 2018 The original author or authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package core

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"github.com/projectriff/riff/pkg/crd"
	"github.com/projectriff/riff/pkg/kubectl"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projectriff/riff/pkg/fileutils"

	"github.com/projectriff/riff/pkg/env"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	istioNamespace = "istio-system"
	serviceResourceName = "service"
	horizontalPodAutoscalerResourceName = "horizontalpodautoscaler"
	deploymentResourceName = "deployment"
)

type SystemInstallOptions struct {
	Manifest string
	NodePort bool
	Force    bool
}

type SystemUninstallOptions struct {
	Istio bool
	Force bool
}

var (
	knativeNamespaces = []string{"knative-eventing", "knative-serving", "knative-build", "knative-monitoring"}
	allNameSpaces     = append(knativeNamespaces, istioNamespace)
)

func (c *client) SystemInstall(manifests map[string]*Manifest, options SystemInstallOptions) (bool, error) {
	manifest, err := ResolveManifest(manifests, options.Manifest)
	if err != nil {
		return false, err
	}

	err = ensureNotTerminating(c, allNameSpaces, "Please try again later.")
	if err != nil {
		return false, err
	}

	err = crd.CreateCRD(c.apiExtension)
	if err != nil {
		return false, errors.New(fmt.Sprintf("Could not create riff CRD: %s ", err))
	}
	riffManifest, err := c.createCRDObject(manifest, options)
	if err != nil {
		return false, errors.New(fmt.Sprintf("Could not install riff: %s ", err))
	}
	fmt.Println("Installing", env.Cli.Name, "components")
	fmt.Println()
	err = c.installAndCheckResources(riffManifest, options)
	if err != nil {
		return false, errors.New(fmt.Sprintf("Could not install riff: %s ", err))
	}
	fmt.Print("Knative components installed\n\n")
	return true, nil
}

func (c *client) createCRDObject(manifest *Manifest, options SystemInstallOptions) (*crd.Manifest, error) {
	crdManifest, err := buildCrdManifest(manifest)
	if err != nil {
		return nil, err
	}
	old, err := c.crdClient.Get()
	if old == nil {
		_, err = c.crdClient.Create(crdManifest)
	} else {
		return nil, errors.New("riff crd already installed")
	}
	return crdManifest, err
}

//TODO this is a stop-gap, remove core.Manifest in favor of crd.Manifest
func buildCrdManifest(manifest *Manifest) (*crd.Manifest, error) {
	err := validateManifest(manifest)
	if err != nil {
		return nil, err
	}
	crdManifest := crd.NewManifest()
	var resource *crd.RiffResources
	for i := range crdManifest.Spec.Resources {
		resource = &crdManifest.Spec.Resources[i]
		if strings.Contains(resource.Path, "istio") {
			resource.Path = manifest.Istio[0]
		} else if strings.Contains(resource.Path, "build") && !strings.Contains(resource.Path, "buildtemplate") {
			resource.Path = getElementContaining(manifest.Knative, "build")
		} else if strings.Contains(resource.Path, "serving") {
			resource.Path = getElementContaining(manifest.Knative, "serving")
		} else if strings.Contains(resource.Path, "eventing") && !strings.Contains(resource.Path, "channel") {
			resource.Path = getElementContaining(manifest.Knative, "eventing")
		} else if strings.Contains(resource.Path, "channel") {
			resource.Path = getElementContaining(manifest.Knative, "channel")
		} else if strings.Contains(resource.Path, "buildtemplate") && !strings.Contains(resource.Path, "cache") {
			resource.Path = getElementContaining(manifest.Knative, "buildtemplate")
		} else if strings.Contains(resource.Path, "cache") {
			resource.Path = manifest.Namespace[0]
		}
	}
	return crdManifest, nil
}

func getElementContaining(array []string, substring string) string {
	for _, s := range array {
		if strings.Contains(s, substring) {
			return s
		}
	}
	return ""
}

func validateManifest(manifest *Manifest) error {
	if len(manifest.Namespace) != 1 {
		return errors.New("invalid buildtemplate specified in manifest")
	}
	return nil
}


func (c *client) installAndCheckResources(manifest *crd.Manifest, options SystemInstallOptions) error {
	for _,resource := range manifest.Spec.Resources {
		err := c.installResource(resource, options)
		if err != nil {
			return err
		}
		err = c.checkResource(resource)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *client) installResource(res crd.RiffResources, options SystemInstallOptions) error {
	if res.Path == "" {
		return errors.New("cannot install anything other than a url yet")
	}
	fmt.Printf("installing %s from %s...", res.Name, res.Path)
	yaml, err := fileutils.Read(res.Path, filepath.Dir(options.Manifest))
	if err != nil {
		return err
	}
	if options.NodePort {
		yaml = bytes.Replace(yaml, []byte("type: LoadBalancer"), []byte("type: NodePort"), -1)
	}
	// TODO HACK: use the RESTClient to do this
	kubectl := kubectl.RealKubeCtl()
	istioLog, err := kubectl.ExecStdin([]string{"apply", "-f", "-"}, &yaml)
	if err != nil {
		fmt.Printf("%s\n", istioLog)
		if strings.Contains(istioLog, "forbidden") {
			fmt.Print(`It looks like you don't have cluster-admin permissions.

To fix this you need to:
 1. Delete the current failed installation using:
      ` + env.Cli.Name + ` system uninstall --istio --force
 2. Give the user account used for installation cluster-admin permissions, you can use the following command:
      kubectl create clusterrolebinding cluster-admin-binding \
        --clusterrole=cluster-admin \
        --user=<install-user>
 3. Re-install ` + env.Cli.Name + `

`)
		}
		return err
	}
	return nil
}

// TODO this only supports checking Pods for phases
func (c *client) checkResource(resource crd.RiffResources) error {
	cnt := 1
	for _, check := range resource.Checks {
		var ready bool
		var err error
		for i := 0; i< 360; i++ {
			if strings.EqualFold(check.Kind, "Pod") {
				ready, err = c.isPodReady(check)
				if err != nil {
					return err
				}
				if ready {
					break
				}
			} else {
				return errors.New("only Kind:Pod supported for resource checks")
			}
			time.Sleep(1 * time.Second)
			cnt++
			if cnt % 5 == 0 {
				fmt.Print(".")
			}
		}
		if !ready {
			return errors.New(fmt.Sprintf("The resource %s did not initialize", resource.Name))
		}
	}
	fmt.Println("done")
	return nil
}

func (c *client) isPodReady(check crd.ResourceChecks) (bool, error) {
	pods := c.kubeClient.CoreV1().Pods(check.Namespace)
	podList, err := pods.List(metav1.ListOptions{
		LabelSelector: convertMapToString(check.Selector.MatchLabels),
	})
	if err != nil {
		return false, err
	}
	for _, pod := range podList.Items {
		if strings.EqualFold(string(pod.Status.Phase), check.Pattern) {
			return true, nil
		}
	}
	return false, nil
}

func convertMapToString(m map[string]string) string {
	var s string
	for k,v := range m {
		s += k + "=" + v + ","
	}
	if last := len(s) - 1; last >= 0 && s[last] == ',' {
		s = s[:last]
	}
	return s
}

func (c *client) SystemUninstall(options SystemUninstallOptions) (bool, error) {

	err := ensureNotTerminating(c, allNameSpaces, "This would indicate that the system was already uninstalled.")
	if err != nil {
		return false, err
	}
	knativeNsCount, err := checkNamespacesExists(c, knativeNamespaces)
	istioNsCount, err := checkNamespacesExists(c, []string{istioNamespace})
	if err != nil {
		return false, err
	}
	if knativeNsCount == 0 {
		fmt.Print("No Knative components for " + env.Cli.Name + " found\n")
	} else {
		if !options.Force {
			answer, err := confirm("Are you sure you want to uninstall the " + env.Cli.Name + " system?")
			if err != nil {
				return false, err
			}
			if !answer {
				return false, nil
			}
		}
		fmt.Printf("Removing %s components\n", env.Cli.Name)
		err = deleteCrds(c, "projectriff.io")
		if err != nil {
			return false, err
		}
		fmt.Printf("Removing Knative for %s components\n", env.Cli.Name)
		err = deleteCrds(c, "knative.dev")
		if err != nil {
			return false, err
		}
		err = deleteClusterRoleBindings(c, "knative-")
		if err != nil {
			return false, err
		}
		err = deleteClusterRoleBindings(c, "build-controller-")
		if err != nil {
			return false, err
		}
		err = deleteClusterRoleBindings(c, "eventing-controller-")
		if err != nil {
			return false, err
		}
		err = deleteClusterRoleBindings(c, "in-memory-channel-")
		if err != nil {
			return false, err
		}
		err = deleteClusterRoles(c, "in-memory-channel-")
		if err != nil {
			return false, err
		}
		err = deleteClusterRoles(c, "knative-")
		if err != nil {
			return false, err
		}
		deleteSingleResource(c, serviceResourceName, "knative-ingressgateway", "istio-system")
		deleteSingleResource(c, horizontalPodAutoscalerResourceName, "knative-ingressgateway", "istio-system")
		deleteSingleResource(c, deploymentResourceName, "knative-ingressgateway", "istio-system")
		err = deleteNamespaces(c, knativeNamespaces)
		if err != nil {
			return false, err
		}
	}
	if istioNsCount == 0 {
		fmt.Print("No Istio components found\n")
	} else {
		if !options.Istio {
			if options.Force {
				return true, nil
			}
			answer, err := confirm("Do you also want to uninstall Istio components?")
			if err != nil {
				return false, err
			}
			if !answer {
				return false, nil
			}
		}
		fmt.Print("Removing Istio components\n")
		err = deleteCrds(c, "istio.io")
		if err != nil {
			return false, err
		}
		err = deleteClusterRoleBindings(c, "istio-")
		if err != nil {
			return false, err
		}
		err = deleteClusterRoles(c, "istio-")
		if err != nil {
			return false, err
		}
		err = deleteNamespaces(c, []string{istioNamespace})
		if err != nil {
			return false, err
		}
		// TODO: remove this once https://github.com/knative/serving/issues/2018 is resolved
		deleteSingleResource(c, "horizontalpodautoscaler.autoscaling", "istio-pilot", "")
	}
	return true, nil
}

func deleteNamespaces(c *client, namespaces []string) error {
	for _, namespace := range namespaces {
		fmt.Printf("Deleting resources defined in: %s\n", namespace)
		err := c.kubeClient.CoreV1().Namespaces().Delete(namespace, getDeleteOptionsWithBackgroundDeletePropagation())
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				fmt.Printf("Namespace \"%s\" was not found\n", namespace)
			} else {
				fmt.Printf("%s", err.Error())
			}
		}
	}
	return nil
}

func getDeleteOptionsWithBackgroundDeletePropagation() *metav1.DeleteOptions {
	background := metav1.DeletePropagationBackground
	return &metav1.DeleteOptions{PropagationPolicy: &background}
}

func deleteSingleResource(c *client, resourceType string, name string, namespace string) error {
	var err error
	var deleteLog string
	fmt.Printf("Deleting %s/%s resource in %s\n", resourceType, name, namespace)

	switch resourceType {
	case serviceResourceName:
		err = c.kubeClient.CoreV1().Services(namespace).Delete(namespace,
			getDeleteOptionsWithBackgroundDeletePropagation())
	case horizontalPodAutoscalerResourceName:
		err = c.kubeClient.AutoscalingV2beta2().HorizontalPodAutoscalers(namespace).Delete(name,
			getDeleteOptionsWithBackgroundDeletePropagation())
	case deploymentResourceName:
		err = c.kubeClient.ExtensionsV1beta1().Deployments(namespace).Delete(name,
			getDeleteOptionsWithBackgroundDeletePropagation())
	}
	if err != nil {
		if !strings.Contains(err.Error(), "NotFound") || !strings.Contains(err.Error(), "not found") {
			fmt.Printf("%s", deleteLog)
		}
	}
	return err
}

func deleteClusterRoles(c *client, prefix string) error {
	fmt.Printf("Deleting ClusterRoles prefixed with %s\n", prefix)

	clusterRoleList, err := c.kubeClient.RbacV1().ClusterRoles().List(metav1.ListOptions{})
	for _, clusterRole := range clusterRoleList.Items {
		if strings.HasPrefix(clusterRole.Name, prefix) {
			err = c.kubeClient.RbacV1().ClusterRoles().Delete(clusterRole.Name, getDeleteOptionsWithBackgroundDeletePropagation())
			if err != nil {
				fmt.Println("error while deleting cluster role", clusterRole.Name)
			}
		}
	}
	return nil
}

func deleteClusterRoleBindings(c *client, prefix string) error {
	fmt.Printf("Deleting ClusterRoleBindings prefixed with %s\n", prefix)

	clusterRoleBindingsList, err := c.kubeClient.RbacV1().ClusterRoleBindings().List(metav1.ListOptions{})
	for _, clusterRoleBinding := range clusterRoleBindingsList.Items {
		if strings.HasPrefix(clusterRoleBinding.Name, prefix) {
			err = c.kubeClient.RbacV1().ClusterRoleBindings().Delete(clusterRoleBinding.Name, getDeleteOptionsWithBackgroundDeletePropagation())
			if err != nil {
				fmt.Println("error while deleting cluster role", clusterRoleBinding.Name)
			}
		}
	}
	return nil
}

func deleteCrds(c *client, suffix string) error {
	fmt.Printf("Deleting CRDs for %s\n", suffix)

	crdList, err := c.apiExtension.ApiextensionsV1beta1().CustomResourceDefinitions().List(metav1.ListOptions{})
	for _, crd := range crdList.Items {
		if strings.HasSuffix(crd.Name, suffix) {
			err = c.apiExtension.ApiextensionsV1beta1().CustomResourceDefinitions().Delete(crd.Name,
				getDeleteOptionsWithBackgroundDeletePropagation())
			if err != nil {
				fmt.Println("error while deleting crd", crd.Name)
			}
		}
	}
	return nil
}

func checkNamespacesExists(c *client, names []string) (int, error) {
	count := 0
	for _, name := range names {
		status, err := getNamespaceStatus(c, name)
		if err != nil {
			return count, err
		}
		if status != "'NotFound'" {
			count = +1
		}
	}
	return count, nil
}

func ensureNotTerminating(c *client, names []string, message string) error {
	for _, name := range names {
		status, err := getNamespaceStatus(c, name)
		if err != nil {
			return err
		}
		if status == "'Terminating'" {
			return errors.New(fmt.Sprintf("The %s namespace is currently 'Terminating'. %s", name, message))
		}
	}
	return nil
}

func getNamespaceStatus(c *client, name string) (string, error) {
	ns, err := c.kubeClient.CoreV1().Namespaces().Get(name, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return "'NotFound'", nil
		}
		return "", err
	}
	nsLog := string(ns.Status.Phase)
	return nsLog, nil
}

func confirm(s string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("%s [y/N]: ", s)
	res, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	if len(res) < 2 {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(res))[0] == 'y'
	return answer, nil
}
