package snapshot

import (
	"fmt"

	"github.com/replicatedhq/kots/pkg/k8s"
	"github.com/replicatedhq/kots/pkg/kotsadm"

	"context"
	"regexp"

	"github.com/pkg/errors"
	kotsadmtypes "github.com/replicatedhq/kots/pkg/kotsadm/types"
	veleroclientv1 "github.com/vmware-tanzu/velero/pkg/generated/clientset/versioned/typed/velero/v1"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kuberneteserrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var (
	dockerImageNameRegex = regexp.MustCompile("(?:([^\\/]+)\\/)?(?:([^\\/]+)\\/)?([^@:\\/]+)(?:[@:](.+))")
)

const (
	VeleroNamespaceConfigMapName = "kotsadm-velero-namespace"
)

type VeleroStatus struct {
	Version   string
	Plugins   []string
	Status    string
	Namespace string

	ResticVersion string
	ResticStatus  string
}

func CheckKotsadmVeleroAccess(ctx context.Context, kotsadmNamespace string) (requiresAccess bool, finalErr error) {
	if kotsadmNamespace == "" {
		finalErr = errors.New("kotsadmNamepsace param is required")
		return
	}

	cfg, err := config.GetConfig()
	if err != nil {
		finalErr = errors.Wrap(err, "failed to get cluster config")
		return
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		finalErr = errors.Wrap(err, "failed to create clientset")
		return
	}

	if k8s.IsKotsadmClusterScoped(ctx, clientset, kotsadmNamespace) {
		return
	}

	veleroConfigMap, err := clientset.CoreV1().ConfigMaps(kotsadmNamespace).Get(ctx, VeleroNamespaceConfigMapName, metav1.GetOptions{})
	if err != nil {
		if !kuberneteserrors.IsNotFound(err) {
			finalErr = errors.Wrap(err, "failed to lookup velero configmap")
			return
		}
		// since this is a minimal rbac installation, kotsadm requires this configmap to know which namespace velero is installed in.
		// so if it's not found, then the user probably hasn't yet run the command that gives kotsadm access to the namespace velero is installed in,
		// which will also (re)generate this configmap
		requiresAccess = true
		return
	}

	if veleroConfigMap.Data == nil {
		requiresAccess = true
		return
	}

	veleroNamespace := veleroConfigMap.Data["veleroNamespace"]
	if veleroNamespace == "" {
		requiresAccess = true
		return
	}

	veleroClient, err := veleroclientv1.NewForConfig(cfg)
	if err != nil {
		finalErr = errors.Wrap(err, "failed to create velero clientset")
		return
	}

	backupStorageLocations, err := veleroClient.BackupStorageLocations(veleroNamespace).List(ctx, metav1.ListOptions{})
	if kuberneteserrors.IsForbidden(err) {
		requiresAccess = true
		return
	}
	if err != nil {
		finalErr = errors.Wrap(err, "failed to list backup storage locations")
		return
	}

	verifiedVeleroNamespace := ""
	for _, backupStorageLocation := range backupStorageLocations.Items {
		if backupStorageLocation.Name == "default" {
			verifiedVeleroNamespace = backupStorageLocation.Namespace
			break
		}
	}

	if verifiedVeleroNamespace == "" {
		requiresAccess = true
		return
	}

	_, err = clientset.RbacV1().Roles(verifiedVeleroNamespace).Get(ctx, "kotsadm-role", metav1.GetOptions{})
	if err != nil {
		requiresAccess = true
		return
	}

	_, err = clientset.RbacV1().RoleBindings(verifiedVeleroNamespace).Get(ctx, "kotsadm-rolebinding", metav1.GetOptions{})
	if err != nil {
		requiresAccess = true
		return
	}

	requiresAccess = false
	return
}

func EnsureVeleroPermissions(ctx context.Context, clientset kubernetes.Interface, veleroNamespace string, kotsadmNamespace string) error {
	cfg, err := config.GetConfig()
	if err != nil {
		return errors.Wrap(err, "failed to get cluster config")
	}

	veleroClient, err := veleroclientv1.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "failed to create velero clientset")
	}

	backupStorageLocations, err := veleroClient.BackupStorageLocations(veleroNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Wrapf(err, "failed to list backupstoragelocations in '%s' namespace", veleroNamespace)
	}

	verifiedVeleroNamespace := ""
	for _, backupStorageLocation := range backupStorageLocations.Items {
		if backupStorageLocation.Name == "default" {
			verifiedVeleroNamespace = backupStorageLocation.Namespace
			break
		}
	}

	if verifiedVeleroNamespace == "" {
		return errors.New(fmt.Sprintf("could not detect velero in '%s' namespace", veleroNamespace))
	}

	if err := kotsadm.EnsureKotsadmRole(verifiedVeleroNamespace, clientset); err != nil {
		return errors.Wrap(err, "failed to ensure kotsadm role")
	}

	if err := kotsadm.EnsureKotsadmRoleBinding(verifiedVeleroNamespace, kotsadmNamespace, clientset); err != nil {
		return errors.Wrap(err, "failed to ensure kotsadm rolebinding")
	}

	return nil
}

func EnsureVeleroNamespaceConfigMap(ctx context.Context, clientset kubernetes.Interface, veleroNamespace string, kotsadmNamespace string) error {
	existingConfigMap, err := clientset.CoreV1().ConfigMaps(kotsadmNamespace).Get(ctx, VeleroNamespaceConfigMapName, metav1.GetOptions{})
	if err != nil {
		if !kuberneteserrors.IsNotFound(err) {
			return errors.Wrap(err, "failed to lookup velero configmap")
		}

		newConfigMap := &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "ConfigMap",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      VeleroNamespaceConfigMapName,
				Namespace: kotsadmNamespace,
				Labels:    kotsadmtypes.GetKotsadmLabels(),
			},
			Data: map[string]string{
				"veleroNamespace": veleroNamespace,
			},
		}

		_, err := clientset.CoreV1().ConfigMaps(kotsadmNamespace).Create(ctx, newConfigMap, metav1.CreateOptions{})
		if err != nil {
			return errors.Wrap(err, "failed to create velero configmap")
		}

		return nil
	}

	if existingConfigMap.Data == nil {
		existingConfigMap.Data = make(map[string]string, 0)
	}
	existingConfigMap.Data["veleroNamespace"] = veleroNamespace

	_, err = clientset.CoreV1().ConfigMaps(kotsadmNamespace).Update(ctx, existingConfigMap, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to update velero configmap")
	}

	return nil
}

// TryGetVeleroNamespaceFromConfigMap in the case of minimal rbac installations, a configmap containing the velero namespace
// will be created once the user gives kotsadm access to velero using the cli
func TryGetVeleroNamespaceFromConfigMap(ctx context.Context, clientset kubernetes.Interface, kotsadmNamespace string) string {
	c, err := clientset.CoreV1().ConfigMaps(kotsadmNamespace).Get(ctx, VeleroNamespaceConfigMapName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	if c.Data == nil {
		return ""
	}
	return c.Data["veleroNamespace"]
}

// DetectVeleroNamespace will detect and validate the velero namespace
// kotsadmNamespace is only required in minimal rbac installations. if empty, cluster scope privilages will be needed to detect and validate velero
func DetectVeleroNamespace(ctx context.Context, clientset kubernetes.Interface, kotsadmNamespace string) (string, error) {
	veleroNamespace := ""
	if kotsadmNamespace != "" {
		veleroNamespace = TryGetVeleroNamespaceFromConfigMap(ctx, clientset, kotsadmNamespace)
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return "", errors.Wrap(err, "failed to get cluster config")
	}

	veleroClient, err := veleroclientv1.NewForConfig(cfg)
	if err != nil {
		return "", errors.Wrap(err, "failed to create velero clientset")
	}

	backupStorageLocations, err := veleroClient.BackupStorageLocations(veleroNamespace).List(ctx, metav1.ListOptions{})
	if kuberneteserrors.IsNotFound(err) {
		return "", nil
	}

	if err != nil {
		// can't detect velero
		return "", nil
	}

	for _, backupStorageLocation := range backupStorageLocations.Items {
		if backupStorageLocation.Name == "default" {
			return backupStorageLocation.Namespace, nil
		}
	}

	return "", nil
}

func DetectVelero(ctx context.Context, kotsadmNamespace string) (*VeleroStatus, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cluster config")
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create clientset")
	}

	veleroNamespace, err := DetectVeleroNamespace(ctx, clientset, kotsadmNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to detect velero namespace")
	}

	if veleroNamespace == "" {
		return nil, nil
	}

	veleroStatus := VeleroStatus{
		Plugins:   []string{},
		Namespace: veleroNamespace,
	}

	possibleDeployments, err := listPossibleVeleroDeployments(ctx, clientset, veleroNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list possible velero deployments")
	}

	for _, deployment := range possibleDeployments {
		for _, initContainer := range deployment.Spec.Template.Spec.InitContainers {
			// the default installation is to name these like "velero-plugin-for-aws"
			veleroStatus.Plugins = append(veleroStatus.Plugins, initContainer.Name)
		}

		matches := dockerImageNameRegex.FindStringSubmatch(deployment.Spec.Template.Spec.Containers[0].Image)
		if len(matches) == 5 {
			status := "NotReady"

			if deployment.Status.AvailableReplicas > 0 {
				status = "Ready"
			}

			veleroStatus.Version = matches[4]
			veleroStatus.Status = status

			goto DeploymentFound
		}
	}
DeploymentFound:

	daemonsets, err := listPossibleResticDaemonsets(ctx, clientset, veleroNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list restic daemonsets")
	}
	for _, daemonset := range daemonsets {
		matches := dockerImageNameRegex.FindStringSubmatch(daemonset.Spec.Template.Spec.Containers[0].Image)
		if len(matches) == 5 {
			status := "NotReady"

			if daemonset.Status.NumberAvailable > 0 {
				if daemonset.Status.NumberUnavailable == 0 {
					status = "Ready"
				}
			}

			veleroStatus.ResticVersion = matches[4]
			veleroStatus.ResticStatus = status

			goto ResticFound
		}
	}
ResticFound:

	return &veleroStatus, nil
}

// listPossibleVeleroDeployments filters with a label selector based on how we've found velero deployed
// using the CLI or the Helm Chart.
func listPossibleVeleroDeployments(ctx context.Context, clientset *kubernetes.Clientset, namespace string) ([]v1.Deployment, error) {
	deployments, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "component=velero",
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list deployments")
	}

	helmDeployments, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=velero",
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list helm deployments")
	}

	return append(deployments.Items, helmDeployments.Items...), nil
}

// listPossibleResticDaemonsets filters with a label selector based on how we've found restic deployed
// using the CLI or the Helm Chart.
func listPossibleResticDaemonsets(ctx context.Context, clientset *kubernetes.Clientset, namespace string) ([]v1.DaemonSet, error) {
	daemonsets, err := clientset.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "component=velero",
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list daemonsets")
	}

	helmDaemonsets, err := clientset.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=velero",
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list helm daemonsets")
	}

	return append(daemonsets.Items, helmDaemonsets.Items...), nil
}

// restartVelero will restart velero (and restic)
func restartVelero(ctx context.Context, kotsadmNamespace string) error {
	cfg, err := config.GetConfig()
	if err != nil {
		return errors.Wrap(err, "failed to get cluster config")
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "failed to create clientset")
	}

	veleroNamespace, err := DetectVeleroNamespace(ctx, clientset, kotsadmNamespace)
	if err != nil {
		return errors.Wrap(err, "failed to detect velero namespace")
	}

	veleroDeployments, err := listPossibleVeleroDeployments(ctx, clientset, veleroNamespace)
	if err != nil {
		return errors.Wrap(err, "failed to list velero deployments")
	}

	for _, veleroDeployment := range veleroDeployments {
		pods, err := clientset.CoreV1().Pods(veleroNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(veleroDeployment.Labels).String(),
		})
		if err != nil {
			return errors.Wrap(err, "failed to list pods in velero deployment")
		}

		for _, pod := range pods.Items {
			if err := clientset.CoreV1().Pods(veleroNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
				return errors.Wrap(err, "failed to delete velero deployment")
			}

		}
	}

	resticDaemonSets, err := listPossibleResticDaemonsets(ctx, clientset, veleroNamespace)
	if err != nil {
		return errors.Wrap(err, "failed to list restic daemonsets")
	}

	for _, resticDaemonSet := range resticDaemonSets {
		pods, err := clientset.CoreV1().Pods(veleroNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(resticDaemonSet.Labels).String(),
		})
		if err != nil {
			return errors.Wrap(err, "failed to list pods in restic daemonset")
		}

		for _, pod := range pods.Items {
			if err := clientset.CoreV1().Pods(veleroNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
				return errors.Wrap(err, "failed to delete restic daemonset")
			}

		}
	}

	return nil
}
