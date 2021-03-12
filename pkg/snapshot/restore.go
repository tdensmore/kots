package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/pkg/auth"
	"github.com/replicatedhq/kots/pkg/k8sutil"
	kotsadmtypes "github.com/replicatedhq/kots/pkg/kotsadm/types"
	snapshottypes "github.com/replicatedhq/kots/pkg/kotsadmsnapshot/types"
	"github.com/replicatedhq/kots/pkg/kotsutil"
	"github.com/replicatedhq/kots/pkg/logger"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	veleroclientv1 "github.com/vmware-tanzu/velero/pkg/generated/clientset/versioned/typed/velero/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

type RestoreInstanceBackupOptions struct {
	BackupName            string
	KubernetesConfigFlags *genericclioptions.ConfigFlags
	WaitForApps           bool
	VeleroNamespace       string
}

type ListInstanceRestoresOptions struct {
	Namespace             string
	KubernetesConfigFlags *genericclioptions.ConfigFlags
}

func RestoreInstanceBackup(ctx context.Context, options RestoreInstanceBackupOptions) (*velerov1.Restore, error) {
	clientset, err := k8sutil.GetClientset(options.KubernetesConfigFlags)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get k8s clientset")
	}

	veleroNamespace := options.VeleroNamespace
	if veleroNamespace == "" {
		var err error
		veleroNamespace, err = DetectVeleroNamespace(ctx, clientset, "")
		if err != nil {
			return nil, errors.Wrap(err, "failed to detect velero namespace")
		}
		if veleroNamespace == "" {
			return nil, errors.New("velero not found")
		}
	}

	// get the backup
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cluster config")
	}

	veleroClient, err := veleroclientv1.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create velero clientset")
	}

	backup, err := veleroClient.Backups(veleroNamespace).Get(ctx, options.BackupName, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to find backup")
	}

	// make sure this is an instance backup
	if backup.Annotations["kots.io/instance"] != "true" {
		return nil, errors.Wrap(err, "backup provided is not an instance backup")
	}

	kotsadmImage, ok := backup.Annotations["kots.io/kotsadm-image"]
	if !ok {
		return nil, errors.Wrap(err, "failed to find kotsadm image annotation")
	}

	kotsadmNamespace, _ := backup.Annotations["kots.io/kotsadm-deploy-namespace"]
	if kotsadmNamespace == "" {
		return nil, errors.Wrap(err, "failed to find kotsadm deploy namespace annotation")
	}

	// make sure backup is restorable/complete
	switch backup.Status.Phase {
	case velerov1.BackupPhaseCompleted:
		break
	case velerov1.BackupPhaseFailed, velerov1.BackupPhasePartiallyFailed:
		return nil, errors.Wrap(err, "cannot restore a failed backup")
	default:
		return nil, errors.Wrap(err, "backup is still in progress")
	}

	log := logger.NewCLILogger()
	log.ActionWithSpinner("Deleting Admin Console")

	// delete all kotsadm objects before creating the restore
	err = k8sutil.DeleteKotsadm(ctx, clientset, kotsadmNamespace, kotsutil.IsKurl(clientset))
	if err != nil {
		log.FinishSpinnerWithError()
		return nil, errors.Wrap(err, "failed to delete kotsadm objects")
	}

	log.FinishSpinner()
	log.ActionWithSpinner("Restoring Admin Console")

	// create a restore for kotsadm objects
	trueVal := true
	restore := &velerov1.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: veleroNamespace,
			Name:      fmt.Sprintf("%s.kotsadm", options.BackupName),
			Annotations: map[string]string{
				"kots.io/instance":                 "true",
				"kots.io/kotsadm-image":            kotsadmImage,
				"kots.io/kotsadm-deploy-namespace": kotsadmNamespace,
			},
		},
		Spec: velerov1.RestoreSpec{
			BackupName: options.BackupName,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					kotsadmtypes.KotsadmKey: kotsadmtypes.KotsadmLabelValue, // restoring applications is in a separate step after kotsadm spins up
				},
			},
			RestorePVs:              &trueVal,
			IncludeClusterResources: &trueVal,
		},
	}

	// delete existing restore object (if exists)
	err = veleroClient.Restores(veleroNamespace).Delete(ctx, restore.ObjectMeta.Name, metav1.DeleteOptions{})
	if err != nil && !strings.Contains(err.Error(), "not found") {
		log.FinishSpinnerWithError()
		return nil, errors.Wrapf(err, "failed to delete restore %s", restore.ObjectMeta.Name)
	}

	// create new restore object
	restore, err = veleroClient.Restores(veleroNamespace).Create(ctx, restore, metav1.CreateOptions{})
	if err != nil {
		log.FinishSpinnerWithError()
		return nil, errors.Wrap(err, "failed to create restore")
	}

	// wait for restore to complete
	restore, err = waitForVeleroRestoreCompleted(ctx, veleroNamespace, restore.ObjectMeta.Name)
	if err != nil {
		if restore != nil {
			errMsg := fmt.Sprintf("Admin Console restore failed with %d errors and %d warnings.", restore.Status.Errors, restore.Status.Warnings)
			log.FinishSpinnerWithError()
			log.ActionWithoutSpinner(errMsg)
			return nil, errors.Wrap(err, errMsg)
		}
		log.FinishSpinnerWithError()
		return nil, errors.Wrap(err, "failed to wait for velero restore completed")
	}

	// wait for kotsadm to start up
	timeout, err := time.ParseDuration("10m")
	if err != nil {
		log.FinishSpinnerWithError()
		return nil, errors.Wrap(err, "failed to parse timeout value")
	}
	kotsadmPodName, err := k8sutil.WaitForKotsadm(clientset, kotsadmNamespace, timeout)
	if err != nil {
		log.FinishSpinnerWithError()
		return nil, errors.Wrap(err, "failed to wait for kotsadm")
	}

	log.FinishSpinner()
	log.ActionWithSpinner("Restoring Applications")

	// initiate kotsadm applications restore
	err = initiateKotsadmApplicationsRestore(options.BackupName, kotsadmNamespace, kotsadmPodName, options.KubernetesConfigFlags, log)
	if err != nil {
		log.FinishSpinnerWithError()
		return nil, errors.Wrap(err, "failed to restore kotsadm applications")
	}

	if options.WaitForApps {
		// wait for applications restore to finish
		err = waitForKotsadmApplicationsRestore(options.BackupName, kotsadmNamespace, kotsadmPodName, options.KubernetesConfigFlags, log)
		if err != nil {
			if _, ok := errors.Cause(err).(*kotsadmtypes.ErrorAppsRestore); ok {
				log.FinishSpinnerWithError()
				return nil, errors.Errorf("failed to restore kotsadm applications: %s", err)
			}
			log.FinishSpinnerWithError()
			return nil, errors.Wrap(err, "failed to wait for kotsadm applications restore")
		}

		log.FinishSpinner()
		log.ActionWithoutSpinner("Restore completed successfully.")
	} else {
		log.FinishSpinner()
		log.ActionWithoutSpinner("Admin Console restored successfully. Applications restore is still in progress.")
	}

	return restore, nil
}

func ListInstanceRestores(ctx context.Context, options ListInstanceRestoresOptions) ([]velerov1.Restore, error) {
	clientset, err := k8sutil.GetClientset(options.KubernetesConfigFlags)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get k8s clientset")
	}
	veleroNamespace, err := DetectVeleroNamespace(ctx, clientset, options.Namespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to detect velero namespace")
	}
	if veleroNamespace == "" {
		return nil, errors.New("velero not found")
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cluster config")
	}

	veleroClient, err := veleroclientv1.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create clientset")
	}

	r, err := veleroClient.Restores(veleroNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list restores")
	}

	restores := []velerov1.Restore{}

	for _, restore := range r.Items {
		if restore.Annotations["kots.io/instance"] != "true" {
			continue
		}

		if options.Namespace != "" && restore.Annotations["kots.io/kotsadm-deploy-namespace"] != options.Namespace {
			continue
		}

		restores = append(restores, restore)
	}

	return restores, nil
}

func waitForVeleroRestoreCompleted(ctx context.Context, veleroNamespace string, restoreName string) (*velerov1.Restore, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cluster config")
	}

	veleroClient, err := veleroclientv1.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create clientset")
	}

	for {
		restore, err := veleroClient.Restores(veleroNamespace).Get(ctx, restoreName, metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrap(err, "failed to get restore")
		}

		switch restore.Status.Phase {
		case velerov1.RestorePhaseCompleted:
			return restore, nil
		case velerov1.RestorePhaseFailed:
			return restore, errors.New("restore failed")
		case velerov1.RestorePhasePartiallyFailed:
			return restore, errors.New("restore partially failed")
		default:
			// in progress
		}

		time.Sleep(time.Second)
	}
}

func initiateKotsadmApplicationsRestore(backupName string, kotsadmNamespace string, kotsadmPodName string, kubernetesConfigFlags *genericclioptions.ConfigFlags, log *logger.CLILogger) error {
	stopCh := make(chan struct{})
	defer close(stopCh)

	localPort, errChan, err := k8sutil.PortForward(kubernetesConfigFlags, 0, 3000, kotsadmNamespace, kotsadmPodName, false, stopCh, log)
	if err != nil {
		return errors.Wrap(err, "failed to start port forwarding")
	}

	go func() {
		select {
		case err := <-errChan:
			if err != nil {
				log.Error(err)
			}
		case <-stopCh:
		}
	}()

	authSlug, err := auth.GetOrCreateAuthSlug(kubernetesConfigFlags, kotsadmNamespace)
	if err != nil {
		return errors.Wrap(err, "failed to get kotsadm auth slug")
	}

	url := fmt.Sprintf("http://localhost:%d/api/v1/snapshot/%s/restore-apps", localPort, backupName)

	requestPayload := map[string]interface{}{
		"restoreAll": true,
	}
	requestBody, err := json.Marshal(requestPayload)
	if err != nil {
		return errors.Wrap(err, "failed to marshal request json")
	}

	newRequest, err := http.NewRequest("POST", url, bytes.NewBuffer(requestBody))
	if err != nil {
		return errors.Wrap(err, "failed to create request")
	}
	newRequest.Header.Add("Authorization", authSlug)

	resp, err := http.DefaultClient.Do(newRequest)
	if err != nil {
		return errors.Wrap(err, "failed to get from kotsadm")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("unexpected status code from %s: %s", url, resp.Status)
	}

	return nil
}

func waitForKotsadmApplicationsRestore(backupName string, kotsadmNamespace string, kotsadmPodName string, kubernetesConfigFlags *genericclioptions.ConfigFlags, log *logger.CLILogger) error {
	stopCh := make(chan struct{})
	defer close(stopCh)

	localPort, errChan, err := k8sutil.PortForward(kubernetesConfigFlags, 0, 3000, kotsadmNamespace, kotsadmPodName, false, stopCh, log)
	if err != nil {
		return errors.Wrap(err, "failed to start port forwarding")
	}

	go func() {
		select {
		case err := <-errChan:
			if err != nil {
				log.Error(err)
			}
		case <-stopCh:
		}
	}()

	authSlug, err := auth.GetOrCreateAuthSlug(kubernetesConfigFlags, kotsadmNamespace)
	if err != nil {
		return errors.Wrap(err, "failed to get kotsadm auth slug")
	}

	url := fmt.Sprintf("http://localhost:%d/api/v1/snapshot/%s/apps-restore-status", localPort, backupName)

	for {
		requestPayload := map[string]interface{}{
			"checkAll": true,
		}
		requestBody, err := json.Marshal(requestPayload)
		if err != nil {
			return errors.Wrap(err, "failed to marshal request json")
		}
		newRequest, err := http.NewRequest("POST", url, bytes.NewBuffer(requestBody))
		if err != nil {
			return errors.Wrap(err, "failed to create request")
		}
		newRequest.Header.Add("Authorization", authSlug)

		resp, err := http.DefaultClient.Do(newRequest)
		if err != nil {
			return errors.Wrap(err, "failed to get from kotsadm")
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return errors.Errorf("unexpected status code from %s: %s", url, resp.Status)
		}

		respBody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return errors.Wrap(err, "failed to read server response")
		}

		type AppRestoreStatus struct {
			AppSlug       string                      `json:"appSlug"`
			RestoreDetail snapshottypes.RestoreDetail `json:"restoreDetail"`
		}
		type AppsRestoreStatusResponse struct {
			Statuses []AppRestoreStatus `json:"statuses"`
			Error    string             `json:"error,omitempty"`
		}
		var appsRestoreStatusResponse AppsRestoreStatusResponse
		if err := json.Unmarshal(respBody, &appsRestoreStatusResponse); err != nil {
			return errors.Wrap(err, "failed to unmarshal response")
		}

		if appsRestoreStatusResponse.Error != "" {
			return errors.New(appsRestoreStatusResponse.Error)
		}

		inProgress := false
		errs := []string{}

		for _, s := range appsRestoreStatusResponse.Statuses {
			switch s.RestoreDetail.Phase {
			case velerov1.RestorePhaseCompleted:
				break
			case velerov1.RestorePhaseFailed, velerov1.RestorePhasePartiallyFailed:
				errMsg := fmt.Sprintf("restore failed for app %s with %d errors and %d warnings", s.AppSlug, len(s.RestoreDetail.Errors), len(s.RestoreDetail.Warnings))
				errs = append(errs, errMsg)
				break
			default:
				inProgress = true
			}
		}

		if !inProgress {
			if len(errs) == 0 {
				return nil
			} else {
				errMsg := strings.Join(errs, " AND ")
				return &kotsadmtypes.ErrorAppsRestore{
					Message: errMsg,
				}
			}
		}

		time.Sleep(time.Second * 2)
	}
}
