package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/kotsadm/pkg/app"
	apptypes "github.com/replicatedhq/kots/kotsadm/pkg/app/types"
	"github.com/replicatedhq/kots/kotsadm/pkg/downstream"
	"github.com/replicatedhq/kots/kotsadm/pkg/logger"
	"github.com/replicatedhq/kots/kotsadm/pkg/store"
	"github.com/replicatedhq/kots/kotsadm/pkg/supportbundle"
	"github.com/replicatedhq/kots/kotsadm/pkg/version"
	kotsv1beta1 "github.com/replicatedhq/kots/kotskinds/apis/kots/v1beta1"
	downstreamtypes "github.com/replicatedhq/kots/pkg/api/downstream/types"
	"github.com/replicatedhq/kots/pkg/kotsutil"
	"go.uber.org/zap"
)

type UpdateDeployResultRequest struct {
	AppID        string `json:"appId"`
	IsError      bool   `json:"isError"`
	DryrunStdout string `json:"dryrunStdout"`
	DryrunStderr string `json:"dryrunStderr"`
	ApplyStdout  string `json:"applyStdout"`
	ApplyStderr  string `json:"applyStderr"`
	RenderError  string `json:"renderError"`
}

type UpdateUndeployResultRequest struct {
	AppID   string `json:"appId"`
	IsError bool   `json:"isError"`
}

type DeployAppVersionRequest struct {
	IsSkipPreflights             bool `json:"isSkipPreflights"`
	ContinueWithFailedPreflights bool `json:"continueWithFailedPreflights"`
}

func (h *Handler) DeployAppVersion(w http.ResponseWriter, r *http.Request) {
	appSlug := mux.Vars(r)["appSlug"]

	request := DeployAppVersionRequest{}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		logger.Error(err)
		w.WriteHeader(400)
		return
	}

	sequence, err := strconv.Atoi(mux.Vars(r)["sequence"])
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	a, err := store.GetStore().GetAppFromSlug(appSlug)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	downstreams, err := store.GetStore().ListDownstreamsForApp(a.ID)
	if err != nil {
		err = errors.Wrap(err, "failed to list downstreams for app")
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	} else if len(downstreams) == 0 {
		err = errors.New("no downstreams for app")
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if request.IsSkipPreflights {
		go func() {
			<-time.After(20 * time.Second)
			license, err := store.GetStore().GetLatestLicenseForApp(a.ID)
			if err != nil {
				logger.Error(err)
				w.WriteHeader(500)
				return
			}

			clusterID := downstreams[0].ClusterID

			currentVersion, err := downstream.GetCurrentVersion(a.ID, clusterID)
			if err != nil {
				err = errors.Wrap(err, "failed to get current downstream version")
				logger.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			err = updatePreflightsReportToReplicatedApp(license, appSlug, clusterID, sequence, true, currentVersion.Status)
			if err != nil {
				err = errors.Wrap(err, "failed to update preflights reports")
				logger.Error(err)
				return
			}
		}()
	}

	if request.ContinueWithFailedPreflights && !request.IsSkipPreflights {
		go func() {
			<-time.After(20 * time.Second)
			license, err := store.GetStore().GetLatestLicenseForApp(a.ID)
			if err != nil {
				logger.Error(err)
				w.WriteHeader(500)
				return
			}

			clusterID := downstreams[0].ClusterID

			currentVersion, err := downstream.GetCurrentVersion(a.ID, clusterID)
			if err != nil {
				err = errors.Wrap(err, "failed to get current downstream version")
				logger.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			err = updatePreflightsReportToReplicatedApp(license, appSlug, clusterID, sequence, false, currentVersion.Status)
			if err != nil {
				err = errors.Wrap(err, "failed to update preflights reports")
				logger.Error(err)
				return
			}
		}()
	}

	if err := downstream.DeleteDownstreamDeployStatus(a.ID, downstreams[0].ClusterID, int64(sequence)); err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := version.DeployVersion(a.ID, int64(sequence)); err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	JSON(w, 204, "")
}

// NOTE: this uses special cluster authorization
func (h *Handler) UpdateDeployResult(w http.ResponseWriter, r *http.Request) {
	auth, err := parseClusterAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	clusterID, err := store.GetStore().GetClusterIDFromDeployToken(auth.Password)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	updateDeployResultRequest := UpdateDeployResultRequest{}
	err = json.NewDecoder(r.Body).Decode(&updateDeployResultRequest)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// sequence really should be passed down to operator and returned from it
	currentSequence, err := downstream.GetCurrentSequence(updateDeployResultRequest.AppID, clusterID)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := createSupportBundle(updateDeployResultRequest.AppID, currentSequence, "", true); err != nil {
		// support bundle is not essential.  keep processing deployment request
		logger.Error(errors.Wrapf(err, "failed to create support bundle for sequence %d after deploying", currentSequence))
	}

	alreadySuccessful, err := downstream.IsDownstreamDeploySuccessful(updateDeployResultRequest.AppID, clusterID, currentSequence)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if alreadySuccessful {
		w.WriteHeader(http.StatusOK)
		return
	}

	downstreamOutput := downstreamtypes.DownstreamOutput{
		DryrunStdout: updateDeployResultRequest.DryrunStdout,
		DryrunStderr: updateDeployResultRequest.DryrunStderr,
		ApplyStdout:  updateDeployResultRequest.ApplyStdout,
		ApplyStderr:  updateDeployResultRequest.ApplyStderr,
		RenderError:  updateDeployResultRequest.RenderError,
	}
	err = downstream.UpdateDownstreamDeployStatus(updateDeployResultRequest.AppID, clusterID, currentSequence, updateDeployResultRequest.IsError, downstreamOutput)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func createSupportBundle(appID string, sequence int64, origin string, inCluster bool) error {
	archivePath, err := ioutil.TempDir("", "kotsadm")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(archivePath)

	err = store.GetStore().GetAppVersionArchive(appID, sequence, archivePath)
	if err != nil {
		return errors.Wrap(err, "failed to get current archive")
	}

	kotsKinds, err := kotsutil.LoadKotsKindsFromPath(archivePath)
	if err != nil {
		return errors.Wrap(err, "failed to load current kotskinds")
	}

	err = supportbundle.CreateRenderedSpec(appID, sequence, origin, inCluster, kotsKinds)
	if err != nil {
		return errors.Wrap(err, "failed to create rendered support bundle spec")
	}

	return nil
}

// NOTE: this uses special cluster authorization
func (h *Handler) UpdateUndeployResult(w http.ResponseWriter, r *http.Request) {
	auth, err := parseClusterAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	_, err = store.GetStore().GetClusterIDFromDeployToken(auth.Password)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	updateUndeployResultRequest := UpdateUndeployResultRequest{}
	err = json.NewDecoder(r.Body).Decode(&updateUndeployResultRequest)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var status apptypes.UndeployStatus
	if updateUndeployResultRequest.IsError {
		status = apptypes.UndeployFailed
	} else {
		status = apptypes.UndeployCompleted
	}

	logger.Info("restore API set undeploy status",
		zap.String("status", string(status)),
		zap.String("appID", updateUndeployResultRequest.AppID))

	foundApp, err := store.GetStore().GetApp(updateUndeployResultRequest.AppID)
	if err != nil {
		err = errors.Wrap(err, "failed to get app")
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if foundApp.RestoreInProgressName != "" {
		go func() {
			<-time.After(20 * time.Second)
			err = app.SetRestoreUndeployStatus(updateUndeployResultRequest.AppID, status)
			if err != nil {
				err = errors.Wrap(err, "failed to set app undeploy status")
				logger.Error(err)
				return
			}
		}()
	}

	w.WriteHeader(http.StatusOK)
	return
}

func updatePreflightsReportToReplicatedApp(license *kotsv1beta1.License, appSlug string, clusterID string, sequence int, skipPreflights bool, installStatus string) error {
	urlValues := url.Values{}

	sequenceToStr := fmt.Sprintf("%d", sequence)
	skipPreflightsToStr := fmt.Sprintf("%t", skipPreflights)

	urlValues.Set("sequence", sequenceToStr)
	urlValues.Set("skipPreflights", skipPreflightsToStr)
	urlValues.Set("installStatus", installStatus)

	url := fmt.Sprintf("%s/preflights/reporting/update/%s/%s?%s", license.Spec.Endpoint, appSlug, clusterID, urlValues.Encode())

	var buf bytes.Buffer
	postReq, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return errors.Wrap(err, "failed to call newrequest")
	}
	postReq.Header.Add("Authorization", license.Spec.LicenseID)
	postReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		return errors.Wrap(err, "failed to send preflights reports")
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		return errors.Errorf("Unexpected status code %d", resp.StatusCode)
	}

	_, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read")
	}

	return nil
}
