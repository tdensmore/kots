package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/pkg/k8sutil"
	"github.com/replicatedhq/kots/pkg/kotsadm"
	kotsadmtypes "github.com/replicatedhq/kots/pkg/kotsadm/types"
	kotsadmversion "github.com/replicatedhq/kots/pkg/kotsadm/version"
	"github.com/replicatedhq/kots/pkg/kotsutil"
	types "github.com/replicatedhq/kots/pkg/snapshot/types"
	"github.com/replicatedhq/kots/pkg/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kuberneteserrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	k8syaml "sigs.k8s.io/yaml"
)

const (
	NFSMinioConfigMapName, NFSMinioSecretName            = "kotsadm-nfs-minio", "kotsadm-nfs-minio-creds"
	NFSMinioDeploymentName, NFSMinioServiceName          = "kotsadm-nfs-minio", "kotsadm-nfs-minio"
	NFSMinioProvider, NFSMinioBucketName, NFSMinioRegion = "aws", "velero", "minio"
	NFSMinioServicePort                                  = 9000
)

type NFSDeployOptions struct {
	Namespace   string
	IsOpenShift bool
	ForceReset  bool
	NFSConfig   types.NFSConfig
}

type ResetNFSError struct {
	Message string
}

func (e ResetNFSError) Error() string {
	return e.Message
}

func DeployNFSMinio(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions) error {
	// nfs minio can be deployed before installing kotsadm or the application (e.g. disaster recovery)
	err := kotsadm.EnsurePrivateKotsadmRegistrySecret(deployOptions.Namespace, registryOptions, clientset)
	if err != nil {
		return errors.Wrap(err, "failed to ensure private kotsadm registry secret")
	}

	// configure NFS mount
	shouldReset, hasMinioConfig, err := shouldResetNFSMount(ctx, clientset, deployOptions, registryOptions)
	if err != nil {
		return errors.Wrap(err, "failed to check if should reset nfs mount")
	}
	if shouldReset {
		if !deployOptions.ForceReset {
			return &ResetNFSError{Message: getNFSResetWarningMsg(deployOptions.NFSConfig.Path)}
		}
		err := resetNFSMount(ctx, clientset, deployOptions, registryOptions)
		if err != nil {
			return errors.Wrap(err, "failed to reset nfs mount")
		}
	}
	if shouldReset || !hasMinioConfig {
		// restart nfs minio to regenerate the config
		err := k8sutil.ScaleDownDeployment(ctx, clientset, deployOptions.Namespace, NFSMinioDeploymentName)
		if err != nil {
			return errors.Wrap(err, "failed to scale down nfs minio")
		}
	}

	// deploy resources
	err = ensureNFSConfigMap(ctx, clientset, deployOptions)
	if err != nil {
		return errors.Wrap(err, "failed to ensure nfs minio secret")
	}
	secret, err := ensureNFSMinioSecret(ctx, clientset, deployOptions.Namespace)
	if err != nil {
		return errors.Wrap(err, "failed to ensure nfs minio secret")
	}
	err = writeMinioKeysSHAFile(ctx, clientset, secret, deployOptions, registryOptions)
	if err != nil {
		return errors.Wrap(err, "failed to write minio keys sha file")
	}
	marshalledSecret, err := k8syaml.Marshal(secret)
	if err != nil {
		return errors.Wrap(err, "failed to marshal nfs minio secret")
	}
	if err := ensureNFSMinioDeployment(ctx, clientset, deployOptions, registryOptions, marshalledSecret); err != nil {
		return errors.Wrap(err, "failed to ensure nfs minio deployment")
	}
	if err := ensureNFSMinioService(ctx, clientset, deployOptions.Namespace); err != nil {
		return errors.Wrap(err, "failed to ensure service")
	}

	return nil
}

func ensureNFSConfigMap(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions) error {
	configmap := nfsConfigMapResource(deployOptions.NFSConfig)

	existingConfigMap, err := clientset.CoreV1().ConfigMaps(deployOptions.Namespace).Get(ctx, configmap.Name, metav1.GetOptions{})
	if err != nil {
		if !kuberneteserrors.IsNotFound(err) {
			return errors.Wrap(err, "failed to get existing configmap")
		}

		_, err := clientset.CoreV1().ConfigMaps(deployOptions.Namespace).Create(ctx, configmap, metav1.CreateOptions{})
		if err != nil {
			return errors.Wrap(err, "failed to create configmap")
		}

		return nil
	}

	existingConfigMap = updateNFSConfigMap(existingConfigMap, configmap)

	_, err = clientset.CoreV1().ConfigMaps(deployOptions.Namespace).Update(ctx, existingConfigMap, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to update deployment")
	}

	return nil
}

func nfsConfigMapResource(nfsConfig types.NFSConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: NFSMinioConfigMapName,
		},
		Data: map[string]string{
			"NFS_PATH":   nfsConfig.Path,
			"NFS_SERVER": nfsConfig.Server,
		},
	}
}

func updateNFSConfigMap(existingConfigMap, desiredConfigMap *corev1.ConfigMap) *corev1.ConfigMap {
	existingConfigMap.Data = desiredConfigMap.Data

	return existingConfigMap
}

func ensureNFSMinioSecret(ctx context.Context, clientset kubernetes.Interface, namespace string) (*corev1.Secret, error) {
	secret := nfsMinioSecretResource()

	existingSecret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		if !kuberneteserrors.IsNotFound(err) {
			return nil, errors.Wrap(err, "failed to get existing secret")
		}

		s, err := clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
		if err != nil {
			return nil, errors.Wrap(err, "failed to create secret")
		}

		return s, nil
	}

	// no patch needed

	return existingSecret, nil
}

func nfsMinioSecretResource() *corev1.Secret {
	accessKey := "kotsadm"
	secretKey := uuid.New().String()

	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: NFSMinioSecretName,
		},
		Data: map[string][]byte{
			"MINIO_ACCESS_KEY": []byte(accessKey),
			"MINIO_SECRET_KEY": []byte(secretKey),
		},
	}
}

func ensureNFSMinioDeployment(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions, marshalledSecret []byte) error {
	secretChecksum := fmt.Sprintf("%x", md5.Sum(marshalledSecret))

	deployment, err := nfsMinioDeploymentResource(clientset, secretChecksum, deployOptions, registryOptions)
	if err != nil {
		return errors.Wrap(err, "failed to get deployment resource")
	}

	existingDeployment, err := clientset.AppsV1().Deployments(deployOptions.Namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
	if err != nil {
		if !kuberneteserrors.IsNotFound(err) {
			return errors.Wrap(err, "failed to get existing deployment")
		}

		_, err = clientset.AppsV1().Deployments(deployOptions.Namespace).Create(ctx, deployment, metav1.CreateOptions{})
		if err != nil {
			return errors.Wrap(err, "failed to create deployment")
		}

		return nil
	}

	existingDeployment = updateNFSMinioDeployment(existingDeployment, deployment)

	_, err = clientset.AppsV1().Deployments(deployOptions.Namespace).Update(ctx, existingDeployment, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to update deployment")
	}

	return nil
}

func nfsMinioDeploymentResource(clientset kubernetes.Interface, secretChecksum string, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions) (*appsv1.Deployment, error) {
	kotsadmTag := kotsadmversion.KotsadmTag(kotsadmtypes.KotsadmOptions{}) // default tag
	image := fmt.Sprintf("kotsadm/minio:%s", kotsadmTag)
	imagePullSecrets := []corev1.LocalObjectReference{}

	if !kotsutil.IsKurl(clientset) || deployOptions.Namespace != metav1.NamespaceDefault {
		var err error
		imageRewriteFn := kotsadmversion.ImageRewriteKotsadmRegistry(deployOptions.Namespace, &registryOptions)
		image, imagePullSecrets, err = imageRewriteFn(image, false)
		if err != nil {
			return nil, errors.Wrap(err, "failed to rewrite image")
		}
	}

	var securityContext corev1.PodSecurityContext
	if !deployOptions.IsOpenShift {
		securityContext = corev1.PodSecurityContext{
			RunAsUser: util.IntPointer(1001),
			FSGroup:   util.IntPointer(1001),
		}
	}

	env := []corev1.EnvVar{
		{
			Name:  "MINIO_UPDATE",
			Value: "off",
		},
		{
			Name: "MINIO_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: NFSMinioSecretName,
					},
					Key: "MINIO_ACCESS_KEY",
				},
			},
		},
		{
			Name: "MINIO_SECRET_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: NFSMinioSecretName,
					},
					Key: "MINIO_SECRET_KEY",
				},
			},
		},
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: NFSMinioDeploymentName,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "kotsadm-nfs-minio",
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "kotsadm-nfs-minio",
					},
					Annotations: map[string]string{
						"kots.io/nfs-minio-creds-secret-checksum": secretChecksum,
					},
				},
				Spec: corev1.PodSpec{
					SecurityContext:  &securityContext,
					ImagePullSecrets: imagePullSecrets,
					Containers: []corev1.Container{
						{
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Name:            "minio",
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: 9000},
							},
							Env: env,
							VolumeMounts: []corev1.VolumeMount{
								{
									Name: "data", MountPath: "/data",
								},
							},
							Args: []string{"--quiet", "server", "data"},
							LivenessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/minio/health/live",
										Port: intstr.FromInt(9000),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       20,
							},
							ReadinessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/minio/health/ready",
										Port: intstr.FromInt(9000),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       20,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								NFS: &corev1.NFSVolumeSource{
									Path:   deployOptions.NFSConfig.Path,
									Server: deployOptions.NFSConfig.Server,
								},
							},
						},
					},
				},
			},
		},
	}, nil
}

func updateNFSMinioDeployment(existingDeployment, desiredDeployment *appsv1.Deployment) *appsv1.Deployment {
	if len(existingDeployment.Spec.Template.Spec.Containers) == 0 {
		// hmm
		return desiredDeployment
	}

	existingDeployment.Spec.Replicas = desiredDeployment.Spec.Replicas

	if existingDeployment.Spec.Template.Annotations == nil {
		existingDeployment.Spec.Template.ObjectMeta.Annotations = map[string]string{}
	}
	existingDeployment.Spec.Template.ObjectMeta.Annotations["kots.io/nfs-minio-creds-secret-checksum"] = desiredDeployment.Spec.Template.ObjectMeta.Annotations["kots.io/nfs-minio-creds-secret-checksum"]

	existingDeployment.Spec.Template.Spec.Containers[0].Image = desiredDeployment.Spec.Template.Spec.Containers[0].Image
	existingDeployment.Spec.Template.Spec.Containers[0].LivenessProbe = desiredDeployment.Spec.Template.Spec.Containers[0].LivenessProbe
	existingDeployment.Spec.Template.Spec.Containers[0].ReadinessProbe = desiredDeployment.Spec.Template.Spec.Containers[0].ReadinessProbe
	existingDeployment.Spec.Template.Spec.Containers[0].Env = desiredDeployment.Spec.Template.Spec.Containers[0].Env

	return existingDeployment
}

func ensureNFSMinioService(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	service := nfsMinioServiceResource()

	existingService, err := clientset.CoreV1().Services(namespace).Get(ctx, service.Name, metav1.GetOptions{})
	if err != nil {
		if !kuberneteserrors.IsNotFound(err) {
			return errors.Wrap(err, "failed to get existing service")
		}

		_, err = clientset.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
		if err != nil {
			return errors.Wrap(err, "failed to create service")
		}

		return nil
	}

	existingService = updateNFSMinioService(existingService, service)

	_, err = clientset.CoreV1().Services(namespace).Update(ctx, existingService, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to update service")
	}

	return nil
}

func nfsMinioServiceResource() *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: NFSMinioServiceName,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"app": "kotsadm-nfs-minio",
			},
			Ports: []corev1.ServicePort{
				{
					Protocol:   corev1.ProtocolTCP,
					Port:       NFSMinioServicePort,
					TargetPort: intstr.FromInt(9000),
				},
			},
		},
	}
}

func updateNFSMinioService(existingService, desiredService *corev1.Service) *corev1.Service {
	existingService.Spec.Ports = desiredService.Spec.Ports

	return existingService
}

func shouldResetNFSMount(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions) (shouldReset bool, hasMinioConfig bool, finalErr error) {
	checkPod, err := createNFSMinioCheckPod(ctx, clientset, deployOptions, registryOptions)
	if err != nil {
		finalErr = errors.Wrap(err, "failed to create nfs minio check pod")
		return
	}

	if err := k8sutil.WaitForPodCompleted(ctx, clientset, deployOptions.Namespace, checkPod.Name, time.Minute*2); err != nil {
		finalErr = errors.Wrap(err, "failed to wait for nfs minio check pod to complete")
		return
	}
	defer clientset.CoreV1().Pods(deployOptions.Namespace).Delete(ctx, checkPod.Name, metav1.DeleteOptions{})

	logs, err := k8sutil.GetPodLogs(ctx, clientset, checkPod)
	if err != nil {
		finalErr = errors.Wrap(err, "failed to get nfs minio check pod logs")
		return
	}
	if len(logs) == 0 {
		finalErr = errors.New("no logs found")
		return
	}

	type NFSMinioCheckPodOutput struct {
		HasMinioConfig bool   `json:"hasMinioConfig"`
		MinioKeysSHA   string `json:"minioKeysSHA"`
	}

	checkPodOutput := NFSMinioCheckPodOutput{}

	scanner := bufio.NewScanner(bytes.NewReader(logs))
	for scanner.Scan() {
		line := scanner.Text()

		if err := json.Unmarshal([]byte(line), &checkPodOutput); err != nil {
			continue
		}

		break
	}

	if !checkPodOutput.HasMinioConfig {
		shouldReset = false
		hasMinioConfig = false
		return
	}

	if checkPodOutput.MinioKeysSHA == "" {
		shouldReset = true
		hasMinioConfig = true
		return
	}

	minioSecret, err := clientset.CoreV1().Secrets(deployOptions.Namespace).Get(ctx, NFSMinioSecretName, metav1.GetOptions{})
	if err != nil {
		if !kuberneteserrors.IsNotFound(err) {
			finalErr = errors.Wrap(err, "failed to get existing minio secret")
			return
		}
		shouldReset = true
		hasMinioConfig = true
		return
	}

	newMinioKeysSHA := getMinioKeysSHA(string(minioSecret.Data["MINIO_ACCESS_KEY"]), string(minioSecret.Data["MINIO_SECRET_KEY"]))
	if newMinioKeysSHA == checkPodOutput.MinioKeysSHA {
		shouldReset = false
		hasMinioConfig = true
		return
	}

	shouldReset = true
	hasMinioConfig = true
	return
}

func resetNFSMount(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions) error {
	resetPod, err := createNFSMinioResetPod(ctx, clientset, deployOptions, registryOptions)
	if err != nil {
		return errors.Wrap(err, "failed to create nfs minio reset pod")
	}

	if err := k8sutil.WaitForPodCompleted(ctx, clientset, deployOptions.Namespace, resetPod.Name, time.Minute*2); err != nil {
		return errors.Wrap(err, "failed to wait for nfs minio reset pod to complete")
	}

	clientset.CoreV1().Pods(deployOptions.Namespace).Delete(ctx, resetPod.Name, metav1.DeleteOptions{})

	return nil
}

func writeMinioKeysSHAFile(ctx context.Context, clientset kubernetes.Interface, minioSecret *corev1.Secret, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions) error {
	minioKeysSHA := getMinioKeysSHA(string(minioSecret.Data["MINIO_ACCESS_KEY"]), string(minioSecret.Data["MINIO_SECRET_KEY"]))

	keysSHAPod, err := createNFSMinioKeysSHAPod(ctx, clientset, deployOptions, registryOptions, minioKeysSHA)
	if err != nil {
		return errors.Wrap(err, "failed to create nfs minio keysSHA pod")
	}

	if err := k8sutil.WaitForPodCompleted(ctx, clientset, deployOptions.Namespace, keysSHAPod.Name, time.Minute*2); err != nil {
		return errors.Wrap(err, "failed to wait for nfs minio keysSHA pod to complete")
	}

	clientset.CoreV1().Pods(deployOptions.Namespace).Delete(ctx, keysSHAPod.Name, metav1.DeleteOptions{})

	return nil
}

func getMinioKeysSHA(accessKey, secretKey string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s,%s", accessKey, secretKey))))
}

func createNFSMinioCheckPod(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions) (*corev1.Pod, error) {
	pod, err := nfsMinioCheckPod(ctx, clientset, deployOptions, registryOptions)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get pod resource")
	}

	p, err := clientset.CoreV1().Pods(deployOptions.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create pod")
	}

	return p, nil
}

func createNFSMinioResetPod(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions) (*corev1.Pod, error) {
	pod, err := nfsMinioResetPod(ctx, clientset, deployOptions, registryOptions)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get pod resource")
	}

	p, err := clientset.CoreV1().Pods(deployOptions.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create pod")
	}

	return p, nil
}

func createNFSMinioKeysSHAPod(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions, minioKeysSHA string) (*corev1.Pod, error) {
	pod, err := nfsMinioKeysSHAPod(ctx, clientset, deployOptions, registryOptions, minioKeysSHA)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get pod resource")
	}

	p, err := clientset.CoreV1().Pods(deployOptions.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create pod")
	}

	return p, nil
}

func nfsMinioCheckPod(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions) (*corev1.Pod, error) {
	podName := fmt.Sprintf("kotsadm-nfs-minio-check-%d", time.Now().Unix())
	command := []string{"/nfs-minio-check.sh"}
	return nfsMinioConfigPod(clientset, deployOptions, registryOptions, podName, command, nil, true)
}

func nfsMinioResetPod(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions) (*corev1.Pod, error) {
	podName := fmt.Sprintf("kotsadm-nfs-minio-reset-%d", time.Now().Unix())
	command := []string{"/nfs-minio-reset.sh"}
	return nfsMinioConfigPod(clientset, deployOptions, registryOptions, podName, command, nil, false)
}

func nfsMinioKeysSHAPod(ctx context.Context, clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions, minioKeysSHA string) (*corev1.Pod, error) {
	podName := fmt.Sprintf("kotsadm-nfs-minio-keys-sha-%d", time.Now().Unix())
	command := []string{"/nfs-minio-keys-sha.sh"}
	args := []string{minioKeysSHA}
	return nfsMinioConfigPod(clientset, deployOptions, registryOptions, podName, command, args, false)
}

func nfsMinioConfigPod(clientset kubernetes.Interface, deployOptions NFSDeployOptions, registryOptions kotsadmtypes.KotsadmOptions, podName string, command []string, args []string, readOnly bool) (*corev1.Pod, error) {
	var securityContext corev1.PodSecurityContext
	if !deployOptions.IsOpenShift {
		securityContext = corev1.PodSecurityContext{
			RunAsUser: util.IntPointer(1001),
			FSGroup:   util.IntPointer(1001),
		}
	}

	kotsadmTag := kotsadmversion.KotsadmTag(kotsadmtypes.KotsadmOptions{}) // default tag
	image := fmt.Sprintf("kotsadm/kotsadm:%s", kotsadmTag)
	imagePullSecrets := []corev1.LocalObjectReference{}

	if !kotsutil.IsKurl(clientset) || deployOptions.Namespace != metav1.NamespaceDefault {
		var err error
		imageRewriteFn := kotsadmversion.ImageRewriteKotsadmRegistry(deployOptions.Namespace, &registryOptions)
		image, imagePullSecrets, err = imageRewriteFn(image, false)
		if err != nil {
			return nil, errors.Wrap(err, "failed to rewrite image")
		}
	}

	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: deployOptions.Namespace,
		},
		Spec: corev1.PodSpec{
			SecurityContext:  &securityContext,
			RestartPolicy:    corev1.RestartPolicyOnFailure,
			ImagePullSecrets: imagePullSecrets,
			Volumes: []corev1.Volume{
				{
					Name: "nfs",
					VolumeSource: corev1.VolumeSource{
						NFS: &corev1.NFSVolumeSource{
							Path:   deployOptions.NFSConfig.Path,
							Server: deployOptions.NFSConfig.Server,
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Image:           "ttl.sh/salah/kotsadm:12h", // TODO NOW: revert this image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Name:            "nfs-minio",
					Command:         command,
					Args:            args,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "nfs",
							MountPath: "/nfs",
							ReadOnly:  readOnly,
						},
					},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							"cpu":    resource.MustParse("100m"),
							"memory": resource.MustParse("100Mi"),
						},
						Requests: corev1.ResourceList{
							"cpu":    resource.MustParse("50m"),
							"memory": resource.MustParse("50Mi"),
						},
					},
				},
			},
		},
	}

	return pod, nil
}

func getNFSResetWarningMsg(nfsPath string) string {
	return fmt.Sprintf("The %s directory was previously configured by a different minio instance.\nProceeding will re-configure it to be used only by the minio instance we deploy to configure NFS, and any other minio instance using this location will no longer have access.\nIf you are attempting to fully restore a prior installation, such as a disaster recovery scenario, this action is expected.", nfsPath)
}

func CreateNFSMinioBucket(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, NFSMinioSecretName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to get nfs minio secret")
	}

	service, err := clientset.CoreV1().Services(namespace).Get(ctx, NFSMinioServiceName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to get nfs minio service")
	}

	endpoint := fmt.Sprintf("http://%s:%d", service.Spec.ClusterIP, service.Spec.Ports[0].Port)
	accessKeyID := string(secret.Data["MINIO_ACCESS_KEY"])
	secretAccessKey := string(secret.Data["MINIO_SECRET_KEY"])

	s3Config := &aws.Config{
		Region:           aws.String(NFSMinioRegion),
		Endpoint:         aws.String(endpoint),
		DisableSSL:       aws.Bool(true), // TODO: this needs to be configurable
		S3ForcePathStyle: aws.Bool(true),
	}

	if accessKeyID != "" && secretAccessKey != "" {
		s3Config.Credentials = credentials.NewStaticCredentials(accessKeyID, secretAccessKey, "")
	}

	newSession := session.New(s3Config)
	s3Client := s3.New(newSession)

	_, err = s3Client.HeadBucket(&s3.HeadBucketInput{
		Bucket: aws.String(NFSMinioBucketName),
	})

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "NotFound" {
				_, err = s3Client.CreateBucket(&s3.CreateBucketInput{
					Bucket: aws.String(NFSMinioBucketName),
				})
				if err != nil {
					return errors.Wrap(err, "failed to create bucket")
				}
			}
		}
		return errors.Wrap(err, "failed to check if bucket exists")
	}

	return nil
}

func GetCurrentNFSConfig(ctx context.Context, namespace string) (*types.NFSConfig, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cluster config")
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create kubernetes clientset")
	}

	nfsConfigMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, NFSMinioConfigMapName, metav1.GetOptions{})
	if err != nil && !kuberneteserrors.IsNotFound(err) {
		return nil, errors.Wrap(err, "failed to get nfs configmap")
	}

	nfsConfig := types.NFSConfig{
		Path:   nfsConfigMap.Data["NFS_PATH"],
		Server: nfsConfigMap.Data["NFS_SERVER"],
	}

	return &nfsConfig, nil
}
