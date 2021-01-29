package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	gcpstorage "cloud.google.com/go/storage"
	storagemgmt "github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2018-02-01/storage"
	"github.com/Azure/azure-sdk-for-go/storage"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/pkg/kotsutil"
	"github.com/replicatedhq/kots/pkg/snapshot/providers"
	"github.com/replicatedhq/kots/pkg/snapshot/types"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	veleroclientv1 "github.com/vmware-tanzu/velero/pkg/generated/clientset/versioned/typed/velero/v1"
	"google.golang.org/api/option"
	"gopkg.in/ini.v1"
	corev1 "k8s.io/api/core/v1"
	kuberneteserrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

type ConfigureStoreOptions struct {
	Provider string
	Bucket   string
	Path     string

	AWS      *types.StoreAWS
	Google   *types.StoreGoogle
	Azure    *types.StoreAzure
	Other    *types.StoreOther
	Internal bool
	NFS      bool

	KotsadmNamespace string
}

type InvalidStoreDataError struct {
	Message string
}

func (e *InvalidStoreDataError) Error() string {
	return e.Message
}

func ConfigureStore(options ConfigureStoreOptions) (*types.Store, error) {
	store, err := GetGlobalStore(nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get store")
	}

	store.Provider = options.Provider
	store.Bucket = options.Bucket
	store.Path = options.Path

	if options.AWS != nil {
		if store.AWS == nil {
			store.AWS = &types.StoreAWS{}
		}
		store.Azure = nil
		store.Google = nil
		store.Other = nil
		store.Internal = nil
		store.NFS = nil

		store.AWS.UseInstanceRole = options.AWS.UseInstanceRole
		if store.AWS.UseInstanceRole {
			store.AWS.AccessKeyID = ""
			store.AWS.SecretAccessKey = ""
		} else {
			if options.AWS.AccessKeyID != "" {
				store.AWS.AccessKeyID = options.AWS.AccessKeyID
			}
			if options.AWS.SecretAccessKey != "" {
				if strings.Contains(options.AWS.SecretAccessKey, "REDACTED") {
					return nil, &InvalidStoreDataError{Message: "invalid aws secret access key"}
				}
				store.AWS.SecretAccessKey = options.AWS.SecretAccessKey
			}
			if options.AWS.Region != "" {
				store.AWS.Region = options.AWS.Region
			}
		}

		if !store.AWS.UseInstanceRole {
			if store.AWS.AccessKeyID == "" || store.AWS.SecretAccessKey == "" || store.AWS.Region == "" {
				return nil, &InvalidStoreDataError{Message: "missing access key id and/or secret access key and/or region"}
			}
		}
	} else if options.Google != nil {
		if store.Google == nil {
			store.Google = &types.StoreGoogle{}
		}
		store.AWS = nil
		store.Azure = nil
		store.Other = nil
		store.Internal = nil
		store.NFS = nil

		store.Google.UseInstanceRole = options.Google.UseInstanceRole
		if store.Google.UseInstanceRole {
			store.Google.JSONFile = ""
			if options.Google.ServiceAccount != "" {
				store.Google.ServiceAccount = options.Google.ServiceAccount
			}
		} else {
			if options.Google.JSONFile != "" {
				if strings.Contains(options.Google.JSONFile, "REDACTED") {
					return nil, &InvalidStoreDataError{Message: "invalid JSON file"}
				}
				store.Google.JSONFile = options.Google.JSONFile
			}
		}

		if store.Google.UseInstanceRole {
			if store.Google.ServiceAccount == "" {
				return nil, &InvalidStoreDataError{Message: "missing service account"}
			}
		} else {
			if store.Google.JSONFile == "" {
				return nil, &InvalidStoreDataError{Message: "missing JSON file"}
			}
		}

	} else if options.Azure != nil {
		if store.Azure == nil {
			store.Azure = &types.StoreAzure{}
		}
		store.AWS = nil
		store.Google = nil
		store.Other = nil
		store.Internal = nil
		store.NFS = nil

		if options.Azure.ResourceGroup != "" {
			store.Azure.ResourceGroup = options.Azure.ResourceGroup
		}
		if options.Azure.SubscriptionID != "" {
			store.Azure.SubscriptionID = options.Azure.SubscriptionID
		}
		if options.Azure.TenantID != "" {
			store.Azure.TenantID = options.Azure.TenantID
		}
		if options.Azure.ClientID != "" {
			store.Azure.ClientID = options.Azure.ClientID
		}
		if options.Azure.ClientSecret != "" {
			if strings.Contains(options.Azure.ClientSecret, "REDACTED") {
				return nil, &InvalidStoreDataError{Message: "invalid client secret"}
			}
			store.Azure.ClientSecret = options.Azure.ClientSecret
		}
		if options.Azure.CloudName != "" {
			store.Azure.CloudName = options.Azure.CloudName
		}
		if options.Azure.StorageAccount != "" {
			store.Azure.StorageAccount = options.Azure.StorageAccount
		}

	} else if options.Other != nil {
		if store.Other == nil {
			store.Other = &types.StoreOther{}
		}
		store.AWS = nil
		store.Google = nil
		store.Azure = nil
		store.Internal = nil
		store.NFS = nil

		store.Provider = "aws"
		if options.Other.AccessKeyID != "" {
			store.Other.AccessKeyID = options.Other.AccessKeyID
		}
		if options.Other.SecretAccessKey != "" {
			if strings.Contains(options.Other.SecretAccessKey, "REDACTED") {
				return nil, &InvalidStoreDataError{Message: "invalid secret access key"}
			}
			store.Other.SecretAccessKey = options.Other.SecretAccessKey
		}
		if options.Other.Region != "" {
			store.Other.Region = options.Other.Region
		}
		if options.Other.Endpoint != "" {
			store.Other.Endpoint = options.Other.Endpoint
		}

		if store.Other.AccessKeyID == "" || store.Other.SecretAccessKey == "" || store.Other.Endpoint == "" || store.Other.Region == "" {
			return nil, &InvalidStoreDataError{Message: "access key, secret key, endpoint and region are required"}
		}
	} else if options.Internal {
		cfg, err := config.GetConfig()
		if err != nil {
			return nil, errors.Wrap(err, "failed to get cluster config")
		}
		clientset, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create clientset")
		}

		if !kotsutil.IsKurl(clientset) {
			return nil, &InvalidStoreDataError{Message: "cannot use internal storage on a non-kurl cluster"}
		}

		if store.Internal == nil {
			store.Internal = &types.StoreInternal{}
		}
		store.AWS = nil
		store.Google = nil
		store.Azure = nil
		store.Other = nil
		store.NFS = nil

		secret, err := kotsutil.GetKurlS3Secret()
		if err != nil {
			return nil, errors.Wrap(err, "failed to get s3 secret")
		}
		if secret == nil {
			return nil, errors.New("s3 secret does not exist")
		}

		store.Provider = "aws"
		store.Bucket = string(secret.Data["velero-local-bucket"])
		store.Path = ""

		store.Internal.AccessKeyID = string(secret.Data["access-key-id"])
		store.Internal.SecretAccessKey = string(secret.Data["secret-access-key"])
		store.Internal.Endpoint = string(secret.Data["endpoint"])
		store.Internal.ObjectStoreClusterIP = string(secret.Data["object-store-cluster-ip"])
		store.Internal.Region = "us-east-1"

	} else if options.NFS {
		store.AWS = nil
		store.Google = nil
		store.Azure = nil
		store.Other = nil
		store.Internal = nil

		store.Provider = NFSMinioProvider
		store.Bucket = NFSMinioBucketName
		store.Path = ""

		cfg, err := config.GetConfig()
		if err != nil {
			return nil, errors.Wrap(err, "failed to get cluster config")
		}

		clientset, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create clientset")
		}

		storeNFS, err := BuildStoreNFS(context.TODO(), clientset, options.KotsadmNamespace)
		if err != nil {
			return nil, errors.Wrap(err, "failed to build nfs store")
		}
		store.NFS = storeNFS
	}

	if err := validateStore(store); err != nil {
		return nil, &InvalidStoreDataError{Message: errors.Cause(err).Error()}
	}

	updatedBackupStorageLocation, err := updateGlobalStore(store)
	if err != nil {
		return nil, errors.Wrap(err, "failed to update global store")
	}

	if err := resetResticRepositories(); err != nil {
		return nil, errors.Wrap(err, "failed to try to reset restic repositories")
	}

	// most plugins (all?) require that velero be restared after updating
	if err := restartVelero(); err != nil {
		return nil, errors.Wrap(err, "failed to try to restart velero")
	}

	updatedStore, err := GetGlobalStore(updatedBackupStorageLocation)
	if err != nil {
		return nil, errors.Wrap(err, "failed to update store")
	}

	if err := Redact(updatedStore); err != nil {
		return nil, errors.Wrap(err, "failed to redact")
	}

	return updatedStore, nil
}

// updateGlobalStore will update the in-cluster storage with exactly what's in the store param
func updateGlobalStore(store *types.Store) (*velerov1.BackupStorageLocation, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cluster config")
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create clientset")
	}

	veleroClient, err := veleroclientv1.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create velero clientset")
	}

	kotsadmVeleroBackendStorageLocation, err := FindBackupStoreLocation()
	if err != nil {
		return nil, errors.Wrap(err, "failed to find backupstoragelocations")
	}

	kotsadmVeleroBackendStorageLocation.Spec.Provider = store.Provider

	if kotsadmVeleroBackendStorageLocation.Spec.ObjectStorage == nil {
		kotsadmVeleroBackendStorageLocation.Spec.ObjectStorage = &velerov1.ObjectStorageLocation{}
	}

	kotsadmVeleroBackendStorageLocation.Spec.ObjectStorage.Bucket = store.Bucket
	kotsadmVeleroBackendStorageLocation.Spec.ObjectStorage.Prefix = store.Path

	currentSecret, currentSecretErr := clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Get(context.TODO(), "cloud-credentials", metav1.GetOptions{})
	if currentSecretErr != nil && !kuberneteserrors.IsNotFound(currentSecretErr) {
		return nil, errors.Wrap(currentSecretErr, "failed to read aws secret")
	}

	if store.AWS != nil {
		kotsadmVeleroBackendStorageLocation.Spec.Config = map[string]string{
			"region": store.AWS.Region,
		}

		if store.AWS.UseInstanceRole {
			// delete the secret
			if currentSecretErr == nil {
				err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Delete(context.TODO(), "cloud-credentials", metav1.DeleteOptions{})
				if err != nil {
					return nil, errors.Wrap(err, "failed to delete aws secret")
				}
			}
		} else {
			awsCredentials, err := BuildAWSCredentials(store.AWS.AccessKeyID, store.AWS.SecretAccessKey)
			if err != nil {
				return nil, errors.Wrap(err, "failed to format aws credentials")
			}

			// create or update the secret
			if kuberneteserrors.IsNotFound(currentSecretErr) {
				// create
				toCreate := corev1.Secret{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Secret",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cloud-credentials",
						Namespace: kotsadmVeleroBackendStorageLocation.Namespace,
					},
					Data: map[string][]byte{
						"cloud": awsCredentials,
					},
				}
				_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Create(context.TODO(), &toCreate, metav1.CreateOptions{})
				if err != nil {
					return nil, errors.Wrap(err, "failed to create aws secret")
				}
			} else {
				// update
				if currentSecret.Data == nil {
					currentSecret.Data = map[string][]byte{}
				}

				currentSecret.Data["cloud"] = awsCredentials
				_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Update(context.TODO(), currentSecret, metav1.UpdateOptions{})
				if err != nil {
					return nil, errors.Wrap(err, "failed to update aws secret")
				}
			}
		}
	} else if store.Other != nil {
		kotsadmVeleroBackendStorageLocation.Spec.Config = map[string]string{
			"region":           store.Other.Region,
			"s3Url":            store.Other.Endpoint,
			"s3ForcePathStyle": "true",
		}

		otherCredentials, err := BuildAWSCredentials(store.Other.AccessKeyID, store.Other.SecretAccessKey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to format other credentials")
		}

		// create or update the secret
		if kuberneteserrors.IsNotFound(currentSecretErr) {
			// create
			toCreate := corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cloud-credentials",
					Namespace: kotsadmVeleroBackendStorageLocation.Namespace,
				},
				Data: map[string][]byte{
					"cloud": otherCredentials,
				},
			}
			_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Create(context.TODO(), &toCreate, metav1.CreateOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "failed to create other secret")
			}
		} else {
			// update
			if currentSecret.Data == nil {
				currentSecret.Data = map[string][]byte{}
			}

			currentSecret.Data["cloud"] = otherCredentials
			_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Update(context.TODO(), currentSecret, metav1.UpdateOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "failed to update other secret")
			}
		}
	} else if store.Internal != nil {
		kotsadmVeleroBackendStorageLocation.Spec.Config = map[string]string{
			"region":           store.Internal.Region,
			"s3Url":            store.Internal.Endpoint,
			"publicUrl":        fmt.Sprintf("http://%s", store.Internal.ObjectStoreClusterIP),
			"s3ForcePathStyle": "true",
		}

		internalCredentials, err := BuildAWSCredentials(store.Internal.AccessKeyID, store.Internal.SecretAccessKey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to format internal credentials")
		}

		// create or update the secret
		if kuberneteserrors.IsNotFound(currentSecretErr) {
			// create
			toCreate := corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cloud-credentials",
					Namespace: kotsadmVeleroBackendStorageLocation.Namespace,
				},
				Data: map[string][]byte{
					"cloud": internalCredentials,
				},
			}
			_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Create(context.TODO(), &toCreate, metav1.CreateOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "failed to create internal secret")
			}
		} else {
			// update
			if currentSecret.Data == nil {
				currentSecret.Data = map[string][]byte{}
			}

			currentSecret.Data["cloud"] = internalCredentials
			_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Update(context.TODO(), currentSecret, metav1.UpdateOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "failed to update internal secret")
			}
		}
	} else if store.NFS != nil {
		kotsadmVeleroBackendStorageLocation.Spec.Config = map[string]string{
			"region":           store.NFS.Region,
			"s3Url":            store.NFS.Endpoint,
			"publicUrl":        getStoreNFSPublicURL(store.NFS),
			"s3ForcePathStyle": "true",
		}

		nfsCredentials, err := BuildAWSCredentials(store.NFS.AccessKeyID, store.NFS.SecretAccessKey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to format nfs credentials")
		}

		// create or update the secret
		if kuberneteserrors.IsNotFound(currentSecretErr) {
			// create
			toCreate := corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cloud-credentials",
					Namespace: kotsadmVeleroBackendStorageLocation.Namespace,
				},
				Data: map[string][]byte{
					"cloud": nfsCredentials,
				},
			}
			_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Create(context.TODO(), &toCreate, metav1.CreateOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "failed to create nfs secret")
			}
		} else {
			// update
			if currentSecret.Data == nil {
				currentSecret.Data = map[string][]byte{}
			}

			currentSecret.Data["cloud"] = nfsCredentials
			_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Update(context.TODO(), currentSecret, metav1.UpdateOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "failed to update nfs secret")
			}
		}
	} else if store.Google != nil {
		if store.Google.UseInstanceRole {
			kotsadmVeleroBackendStorageLocation.Spec.Config["serviceAccount"] = store.Google.ServiceAccount

			// delete the secret
			if currentSecretErr == nil {
				err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Delete(context.TODO(), "cloud-credentials", metav1.DeleteOptions{})
				if err != nil {
					return nil, errors.Wrap(err, "failed to delete google secret")
				}
			}
		} else {
			delete(kotsadmVeleroBackendStorageLocation.Spec.Config, "serviceAccount")

			// create or update the secret
			if kuberneteserrors.IsNotFound(currentSecretErr) {
				// create
				toCreate := corev1.Secret{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Secret",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cloud-credentials",
						Namespace: kotsadmVeleroBackendStorageLocation.Namespace,
					},
					Data: map[string][]byte{
						"cloud": []byte(store.Google.JSONFile),
					},
				}
				_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Create(context.TODO(), &toCreate, metav1.CreateOptions{})
				if err != nil {
					return nil, errors.Wrap(err, "failed to create google secret")
				}
			} else {
				// update
				if currentSecret.Data == nil {
					currentSecret.Data = map[string][]byte{}
				}

				currentSecret.Data["cloud"] = []byte(store.Google.JSONFile)
				_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Update(context.TODO(), currentSecret, metav1.UpdateOptions{})
				if err != nil {
					return nil, errors.Wrap(err, "failed to update google secret")
				}
			}
		}
	} else if store.Azure != nil {
		kotsadmVeleroBackendStorageLocation.Spec.Config["resourceGroup"] = store.Azure.ResourceGroup
		kotsadmVeleroBackendStorageLocation.Spec.Config["storageAccount"] = store.Azure.StorageAccount
		kotsadmVeleroBackendStorageLocation.Spec.Config["subscriptionId"] = store.Azure.SubscriptionID

		config := providers.Azure{
			SubscriptionID: store.Azure.SubscriptionID,
			TenantID:       store.Azure.TenantID,
			ClientID:       store.Azure.ClientID,
			ClientSecret:   store.Azure.ClientSecret,
			ResourceGroup:  store.Azure.ResourceGroup,
			CloudName:      store.Azure.CloudName,
		}

		// create or update the secret
		if kuberneteserrors.IsNotFound(currentSecretErr) {
			// create
			toCreate := corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cloud-credentials",
					Namespace: kotsadmVeleroBackendStorageLocation.Namespace,
				},
				Data: map[string][]byte{
					"cloud": providers.RenderAzureConfig(config),
				},
			}
			_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Create(context.TODO(), &toCreate, metav1.CreateOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "failed to create azure secret")
			}
		} else {
			// update
			if currentSecret.Data == nil {
				currentSecret.Data = map[string][]byte{}
			}

			currentSecret.Data["cloud"] = providers.RenderAzureConfig(config)
			_, err = clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Update(context.TODO(), currentSecret, metav1.UpdateOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "failed to update azure secret")
			}
		}
	}

	updated, err := veleroClient.BackupStorageLocations(kotsadmVeleroBackendStorageLocation.Namespace).Update(context.TODO(), kotsadmVeleroBackendStorageLocation, metav1.UpdateOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to update backup storage location")
	}

	return updated, nil
}

// GetGlobalStore will return the global store from kotsadmVeleroBackupStorageLocation
// or will find it, is the param is nil
func GetGlobalStore(kotsadmVeleroBackendStorageLocation *velerov1.BackupStorageLocation) (*types.Store, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cluster config")
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create clientset")
	}

	if kotsadmVeleroBackendStorageLocation == nil {
		veleroClient, err := veleroclientv1.NewForConfig(cfg)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create velero clientset")
		}

		backupStorageLocations, err := veleroClient.BackupStorageLocations("").List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, errors.Wrap(err, "failed to list backupstoragelocations")
		}

		for _, backupStorageLocation := range backupStorageLocations.Items {
			if backupStorageLocation.Name == "default" {
				kotsadmVeleroBackendStorageLocation = &backupStorageLocation
				break
			}
		}
	}

	if kotsadmVeleroBackendStorageLocation.Spec.ObjectStorage == nil {
		return nil, nil
	}

	prefix := kotsadmVeleroBackendStorageLocation.Spec.ObjectStorage.Prefix

	store := types.Store{
		Provider: kotsadmVeleroBackendStorageLocation.Spec.Provider,
		Bucket:   kotsadmVeleroBackendStorageLocation.Spec.ObjectStorage.Bucket,
		Path:     prefix,
	}

	switch store.Provider {
	case "aws":
		err := mapAWSBackupStorageLocationToStore(kotsadmVeleroBackendStorageLocation, &store)
		if err != nil {
			return nil, errors.Wrap(err, "failed to map aws backup storage location to store")
		}

		awsSecret, err := clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Get(context.TODO(), "cloud-credentials", metav1.GetOptions{})
		if err != nil && !kuberneteserrors.IsNotFound(err) {
			return nil, errors.Wrap(err, "failed to read aws secret")
		}

		if kuberneteserrors.IsNotFound(err) {
			_, isS3Compatible := kotsadmVeleroBackendStorageLocation.Spec.Config["s3Url"]
			if !isS3Compatible {
				store.AWS.UseInstanceRole = true
			}
		} else if err == nil {
			awsCfg, err := ini.Load(awsSecret.Data["cloud"])
			if err != nil {
				return nil, errors.Wrap(err, "failed to load aws credentials")
			}

			for _, section := range awsCfg.Sections() {
				if section.Name() == "default" {
					if store.Internal != nil {
						store.Internal.AccessKeyID = section.Key("aws_access_key_id").Value()
						store.Internal.SecretAccessKey = section.Key("aws_secret_access_key").Value()
					} else if store.NFS != nil {
						store.NFS.AccessKeyID = section.Key("aws_access_key_id").Value()
						store.NFS.SecretAccessKey = section.Key("aws_secret_access_key").Value()
					} else if store.Other != nil {
						store.Other.AccessKeyID = section.Key("aws_access_key_id").Value()
						store.Other.SecretAccessKey = section.Key("aws_secret_access_key").Value()
					} else if store.AWS != nil {
						store.AWS.AccessKeyID = section.Key("aws_access_key_id").Value()
						store.AWS.SecretAccessKey = section.Key("aws_secret_access_key").Value()
					}
				}
			}
		}

		break

	case "azure":
		// TODO validate these keys in a real azure account
		store.Azure = &types.StoreAzure{
			ResourceGroup:  kotsadmVeleroBackendStorageLocation.Spec.Config["resourceGroup"],
			StorageAccount: kotsadmVeleroBackendStorageLocation.Spec.Config["storageAccount"],
			SubscriptionID: kotsadmVeleroBackendStorageLocation.Spec.Config["subscriptionId"],
		}

		// get the secret
		azureSecret, err := clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Get(context.TODO(), "cloud-credentials", metav1.GetOptions{})
		if err != nil && !kuberneteserrors.IsNotFound(err) {
			return nil, errors.Wrap(err, "failed to read azure secret")
		}

		if err == nil {
			azureConfig := providers.ParseAzureConfig(azureSecret.Data["cloud"])
			store.Azure.TenantID = azureConfig.TenantID
			store.Azure.ClientID = azureConfig.ClientID
			store.Azure.ClientSecret = azureConfig.ClientSecret
			store.Azure.CloudName = azureConfig.CloudName
		}

		if store.Azure.CloudName == "" {
			store.Azure.CloudName = "AzurePublicCloud"
		}

		break

	case "gcp":
		currentSecret, err := clientset.CoreV1().Secrets(kotsadmVeleroBackendStorageLocation.Namespace).Get(context.TODO(), "cloud-credentials", metav1.GetOptions{})
		if err != nil && !kuberneteserrors.IsNotFound(err) {
			return nil, errors.Wrap(err, "failed to read google secret")
		}

		jsonFile := ""
		if err == nil {
			currentJSONFile, ok := currentSecret.Data["cloud"]
			if ok {
				jsonFile = string(currentJSONFile)
			}
		}

		store.Google = &types.StoreGoogle{
			ServiceAccount:  kotsadmVeleroBackendStorageLocation.Spec.Config["serviceAccount"],
			JSONFile:        jsonFile,
			UseInstanceRole: jsonFile == "",
		}
		break
	}

	return &store, nil
}

func mapAWSBackupStorageLocationToStore(kotsadmVeleroBackendStorageLocation *velerov1.BackupStorageLocation, store *types.Store) error {
	endpoint, isS3Compatible := kotsadmVeleroBackendStorageLocation.Spec.Config["s3Url"]

	if !isS3Compatible {
		store.AWS = &types.StoreAWS{
			Region: kotsadmVeleroBackendStorageLocation.Spec.Config["region"],
		}
		return nil
	}

	// check if using kurl internal store
	s3Secret, err := kotsutil.GetKurlS3Secret()
	if err != nil {
		return errors.Wrap(err, "failed to get s3 secret")
	}
	if s3Secret != nil && string(s3Secret.Data["endpoint"]) == endpoint {
		store.Internal = &types.StoreInternal{
			Region:               kotsadmVeleroBackendStorageLocation.Spec.Config["region"],
			Endpoint:             endpoint,
			ObjectStoreClusterIP: string(s3Secret.Data["object-store-cluster-ip"]),
		}
		return nil
	}

	// check if using nfs store
	u, err := url.Parse(endpoint)
	if err != nil {
		return errors.Wrap(err, "failed to parse s3 url")
	}
	serviceName := strings.Split(u.Hostname(), ".")[0]
	if u.Scheme == "http" && serviceName == NFSMinioServiceName {
		publicURL, ok := kotsadmVeleroBackendStorageLocation.Spec.Config["publicUrl"]
		if !ok {
			return errors.New("public url for NFS store not found")
		}
		u, err := url.Parse(publicURL)
		if err != nil {
			return errors.Wrap(err, "failed to parse public url for NFS store")
		}
		store.NFS = &types.StoreNFS{
			Region:               kotsadmVeleroBackendStorageLocation.Spec.Config["region"],
			Endpoint:             endpoint,
			ObjectStoreClusterIP: u.Hostname(),
		}
		return nil
	}

	store.Other = &types.StoreOther{
		Region:   kotsadmVeleroBackendStorageLocation.Spec.Config["region"],
		Endpoint: endpoint,
	}

	return nil
}

func FindBackupStoreLocation() (*velerov1.BackupStorageLocation, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cluster config")
	}

	veleroClient, err := veleroclientv1.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create velero clientset")
	}

	backupStorageLocations, err := veleroClient.BackupStorageLocations("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list backupstoragelocations")
	}

	for _, backupStorageLocation := range backupStorageLocations.Items {
		if backupStorageLocation.Name == "default" {
			return &backupStorageLocation, nil
		}
	}

	return nil, errors.New("global config not found")
}

func BuildAWSCredentials(accessKeyID, secretAccessKey string) ([]byte, error) {
	awsCfg := ini.Empty()
	section, err := awsCfg.NewSection("default")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create default section in aws creds")
	}
	_, err = section.NewKey("aws_access_key_id", accessKeyID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create access key")
	}

	_, err = section.NewKey("aws_secret_access_key", secretAccessKey)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create secret access key")
	}

	var awsCredentials bytes.Buffer
	writer := bufio.NewWriter(&awsCredentials)
	_, err = awsCfg.WriteTo(writer)
	if err != nil {
		return nil, errors.Wrap(err, "failed to write ini")
	}
	if err := writer.Flush(); err != nil {
		return nil, errors.Wrap(err, "failed to flush buffer")
	}

	return awsCredentials.Bytes(), nil
}

func BuildStoreNFS(ctx context.Context, clientset kubernetes.Interface, kotsadmNamespace string) (*types.StoreNFS, error) {
	secret, err := clientset.CoreV1().Secrets(kotsadmNamespace).Get(ctx, NFSMinioSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get nfs minio secret")
	}

	service, err := clientset.CoreV1().Services(kotsadmNamespace).Get(ctx, NFSMinioServiceName, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get nfs minio service")
	}

	storeNFS := types.StoreNFS{}
	storeNFS.AccessKeyID = string(secret.Data["MINIO_ACCESS_KEY"])
	storeNFS.SecretAccessKey = string(secret.Data["MINIO_SECRET_KEY"])
	storeNFS.Endpoint = fmt.Sprintf("http://%s.%s:%d", NFSMinioServiceName, kotsadmNamespace, service.Spec.Ports[0].Port)
	storeNFS.ObjectStoreClusterIP = service.Spec.ClusterIP
	storeNFS.Region = NFSMinioRegion

	return &storeNFS, nil
}

func getStoreNFSPublicURL(storeNFS *types.StoreNFS) string {
	return fmt.Sprintf("http://%s:%d", storeNFS.ObjectStoreClusterIP, NFSMinioServicePort)
}

func validateStore(store *types.Store) error {
	if store.AWS != nil {
		if err := validateAWS(store.AWS, store.Bucket); err != nil {
			return errors.Wrap(err, "failed to validate AWS configuration")
		}
		return nil
	}

	if store.Azure != nil {
		if err := validateAzure(store.Azure, store.Bucket); err != nil {
			return errors.Wrap(err, "failed to validate Azure configuration")
		}
		return nil
	}

	if store.Google != nil {
		if err := validateGCP(store.Google, store.Bucket); err != nil {
			return errors.Wrap(err, "failed to validate GCP configuration")
		}
		return nil
	}

	if store.Other != nil {
		if err := validateOther(store.Other, store.Bucket); err != nil {
			return errors.Wrap(err, "failed to validate S3-compatible configuration")
		}
		return nil
	}

	if store.Internal != nil {
		if err := validateInternal(store.Internal, store.Bucket); err != nil {
			return errors.Wrap(err, "failed to validate Internal configuration")
		}
		return nil
	}

	if store.NFS != nil {
		if err := validateNFS(store.NFS, store.Bucket); err != nil {
			return errors.Wrap(err, "failed to validate NFS configuration")
		}
		return nil
	}

	return errors.New("no valid configuration found")
}

func validateAWS(storeAWS *types.StoreAWS, bucket string) error {
	s3Config := &aws.Config{
		Region:           aws.String(storeAWS.Region),
		DisableSSL:       aws.Bool(false),
		S3ForcePathStyle: aws.Bool(false), // TODO: this may need to be configurable
	}

	if storeAWS.UseInstanceRole {
		s3Config.Credentials = credentials.NewChainCredentials([]credentials.Provider{
			&ec2rolecreds.EC2RoleProvider{
				Client:       ec2metadata.New(session.New()),
				ExpiryWindow: 5 * time.Minute,
			},
		})
	} else {
		s3Config.Credentials = credentials.NewStaticCredentials(storeAWS.AccessKeyID, storeAWS.SecretAccessKey, "")
	}

	newSession := session.New(s3Config)
	s3Client := s3.New(newSession)

	_, err := s3Client.HeadBucket(&s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})

	if err != nil {
		return errors.Wrap(err, "bucket does not exist")
	}

	return nil
}

func validateAzure(storeAzure *types.StoreAzure, bucket string) error {
	// Mostly copied from Velero Azure plugin

	env, err := azure.EnvironmentFromName(storeAzure.CloudName)
	if err != nil {
		return errors.Wrap(err, "failed to find azure env")
	}

	oauthConfig, err := adal.NewOAuthConfig(env.ActiveDirectoryEndpoint, storeAzure.TenantID)
	if err != nil {
		return errors.Wrap(err, "failed to get OAuthConfig")
	}

	spt, err := adal.NewServicePrincipalToken(*oauthConfig, storeAzure.ClientID, storeAzure.ClientSecret, env.ResourceManagerEndpoint)
	if err != nil {
		return errors.Wrap(err, "failed to get service principal token")
	}

	storageAccountsClient := storagemgmt.NewAccountsClientWithBaseURI(env.ResourceManagerEndpoint, storeAzure.SubscriptionID)
	storageAccountsClient.Authorizer = autorest.NewBearerAuthorizer(spt)

	res, err := storageAccountsClient.ListKeys(context.TODO(), storeAzure.ResourceGroup, storeAzure.StorageAccount)
	if err != nil {
		return errors.Wrap(err, "failed to list account keys")
	}
	if res.Keys == nil || len(*res.Keys) == 0 {
		return errors.New("No storage keys found")
	}

	var storageKey string
	for _, key := range *res.Keys {
		// uppercase both strings for comparison because the ListKeys call returns e.g. "FULL" but
		// the storagemgmt.Full constant in the SDK is defined as "Full".
		if strings.ToUpper(string(key.Permissions)) == strings.ToUpper(string(storagemgmt.Full)) {
			storageKey = *key.Value
			break
		}
	}

	if storageKey == "" {
		return errors.New("No storage key with Full permissions found")
	}

	storageClient, err := storage.NewBasicClientOnSovereignCloud(storeAzure.StorageAccount, storageKey, env)
	if err != nil {
		return errors.Wrap(err, "failed to get storage client")
	}

	blobClient := storageClient.GetBlobService()
	container := blobClient.GetContainerReference(bucket)
	if container == nil {
		return errors.Errorf("unable to get container reference for bucket %s", bucket)
	}

	exists, err := container.Exists()
	if err != nil {
		return errors.Wrap(err, "failed to check container existence")
	}

	if !exists {
		return errors.New("container does not exist")
	}

	return nil
}

func validateGCP(storeGoogle *types.StoreGoogle, bucket string) error {
	ctx := context.Background()
	if storeGoogle.UseInstanceRole {
		// TODO: validate IAM access
	} else {
		client, err := gcpstorage.NewClient(ctx, option.WithCredentialsJSON([]byte(storeGoogle.JSONFile)))
		if err != nil {
			return errors.Wrap(err, "failed to create storage client")
		}

		objectsItr := client.Bucket(bucket).Objects(ctx, &gcpstorage.Query{})
		_, err = objectsItr.Next()
		if err != nil {
			if strings.Contains(err.Error(), "no more items in iterator") {
				return nil
			}

			return errors.Wrap(err, "failed to get bucket attributes")
		}
	}

	return nil
}

func validateOther(storeOther *types.StoreOther, bucket string) error {
	s3Config := &aws.Config{
		Region:           aws.String(storeOther.Region),
		Endpoint:         aws.String(storeOther.Endpoint),
		DisableSSL:       aws.Bool(true), // TODO: this needs to be configurable
		S3ForcePathStyle: aws.Bool(true), // TODO: this may need to be configurable
	}

	if storeOther.AccessKeyID != "" && storeOther.SecretAccessKey != "" {
		s3Config.Credentials = credentials.NewStaticCredentials(storeOther.AccessKeyID, storeOther.SecretAccessKey, "")
	}

	newSession := session.New(s3Config)
	s3Client := s3.New(newSession)

	_, err := s3Client.HeadBucket(&s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})

	if err != nil {
		return errors.Wrap(err, "bucket does not exist")
	}

	return nil
}

func validateInternal(storeInternal *types.StoreInternal, bucket string) error {
	s3Config := &aws.Config{
		Region:           aws.String(storeInternal.Region),
		Endpoint:         aws.String(storeInternal.Endpoint),
		DisableSSL:       aws.Bool(true), // TODO: this needs to be configurable
		S3ForcePathStyle: aws.Bool(true),
	}

	if storeInternal.AccessKeyID != "" && storeInternal.SecretAccessKey != "" {
		s3Config.Credentials = credentials.NewStaticCredentials(storeInternal.AccessKeyID, storeInternal.SecretAccessKey, "")
	}

	newSession := session.New(s3Config)
	s3Client := s3.New(newSession)

	_, err := s3Client.HeadBucket(&s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})

	if err != nil {
		return errors.Wrap(err, "bucket does not exist")
	}

	return nil
}

func validateNFS(storeNFS *types.StoreNFS, bucket string) error {
	// we use publicUrl instead of endpoint since it has to be reachable from the CLI too
	publicURL := getStoreNFSPublicURL(storeNFS)

	s3Config := &aws.Config{
		Region:           aws.String(storeNFS.Region),
		Endpoint:         aws.String(publicURL),
		DisableSSL:       aws.Bool(true), // TODO: this needs to be configurable
		S3ForcePathStyle: aws.Bool(true),
	}

	if storeNFS.AccessKeyID != "" && storeNFS.SecretAccessKey != "" {
		s3Config.Credentials = credentials.NewStaticCredentials(storeNFS.AccessKeyID, storeNFS.SecretAccessKey, "")
	}

	newSession := session.New(s3Config)
	s3Client := s3.New(newSession)

	_, err := s3Client.HeadBucket(&s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})

	if err != nil {
		return errors.Wrap(err, "bucket does not exist")
	}

	return nil
}

func Redact(store *types.Store) error {
	if store == nil {
		return nil
	}

	if store.AWS != nil {
		if store.AWS.SecretAccessKey != "" {
			store.AWS.SecretAccessKey = "--- REDACTED ---"
		}
	}

	if store.Google != nil {
		if store.Google.JSONFile != "" {
			store.Google.JSONFile = "--- REDACTED ---"
		}
	}

	if store.Azure != nil {
		if store.Azure.ClientSecret != "" {
			store.Azure.ClientSecret = "--- REDACTED ---"
		}
	}

	if store.Other != nil {
		if store.Other.SecretAccessKey != "" {
			store.Other.SecretAccessKey = "--- REDACTED ---"
		}
	}

	if store.Internal != nil {
		if store.Internal.SecretAccessKey != "" {
			store.Internal.SecretAccessKey = "--- REDACTED ---"
		}
	}

	if store.NFS != nil {
		if store.NFS.SecretAccessKey != "" {
			store.NFS.SecretAccessKey = "--- REDACTED ---"
		}
	}

	return nil
}

func resetResticRepositories() error {
	// ResticRepositories store the previous snapshot location which breaks volume backup when location changes.
	cfg, err := config.GetConfig()
	if err != nil {
		return errors.Wrap(err, "failed to get cluster config")
	}

	storageLocation, err := FindBackupStoreLocation()
	if err != nil {
		return errors.Wrap(err, "failed to find backupstoragelocations")
	}

	veleroClient, err := veleroclientv1.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "failed to create clientset")
	}

	repos, err := veleroClient.ResticRepositories(storageLocation.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "velero.io/storage-location=default",
	})
	if err != nil {
		return errors.Wrap(err, "failed to list resticrepositories")
	}

	for _, repo := range repos.Items {
		err := veleroClient.ResticRepositories(storageLocation.Namespace).Delete(context.TODO(), repo.Name, metav1.DeleteOptions{})
		if err != nil {
			return errors.Wrapf(err, "failed to delete resticrepository %s", repo.Name)
		}
	}

	return nil
}
