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

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/kotsadm/pkg/logger"
	"github.com/replicatedhq/kots/kotsadm/pkg/preflight"
	preflighttypes "github.com/replicatedhq/kots/kotsadm/pkg/preflight/types"
	"github.com/replicatedhq/kots/kotsadm/pkg/store"
	kotsv1beta1 "github.com/replicatedhq/kots/kotskinds/apis/kots/v1beta1"
	"github.com/replicatedhq/kots/pkg/kotsutil"
)

type GetPreflightResultResponse struct {
	PreflightResult preflighttypes.PreflightResult `json:"preflightResult"`
}

type GetPreflightCommandRequest struct {
	Origin string `json:"origin"`
}

type GetPreflightCommandResponse struct {
	Command []string `json:"command"`
}

func (h *Handler) GetPreflightResult(w http.ResponseWriter, r *http.Request) {
	appSlug := mux.Vars(r)["appSlug"]
	sequence, err := strconv.ParseInt(mux.Vars(r)["sequence"], 10, 64)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(400)
		return
	}

	foundApp, err := store.GetStore().GetAppFromSlug(appSlug)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	result, err := store.GetStore().GetPreflightResults(foundApp.ID, sequence)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	response := GetPreflightResultResponse{
		PreflightResult: *result,
	}
	JSON(w, 200, response)
}

func (h *Handler) GetLatestPreflightResultsForSequenceZero(w http.ResponseWriter, r *http.Request) {
	result, err := store.GetStore().GetLatestPreflightResultsForSequenceZero()
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	response := GetPreflightResultResponse{
		PreflightResult: *result,
	}
	JSON(w, 200, response)
}

func (h *Handler) IgnorePreflightRBACErrors(w http.ResponseWriter, r *http.Request) {
	appSlug := mux.Vars(r)["appSlug"]
	sequence, err := strconv.Atoi(mux.Vars(r)["sequence"])
	if err != nil {
		logger.Error(err)
		w.WriteHeader(400)
		return
	}

	foundApp, err := store.GetStore().GetAppFromSlug(appSlug)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	if err := store.GetStore().SetIgnorePreflightPermissionErrors(foundApp.ID, int64(sequence)); err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	archiveDir, err := ioutil.TempDir("", "kotsadm")
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	removeArchiveDir := true
	defer func() {
		if removeArchiveDir {
			os.RemoveAll(archiveDir)
		}
	}()

	err = store.GetStore().GetAppVersionArchive(foundApp.ID, int64(sequence), archiveDir)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	removeArchiveDir = false
	go func() {
		defer os.RemoveAll(archiveDir)
		if err := preflight.Run(foundApp.ID, foundApp.Slug, int64(sequence), foundApp.IsAirgap, archiveDir); err != nil {
			logger.Error(err)
			return
		}
	}()

	JSON(w, 200, struct{}{})
}

func (h *Handler) StartPreflightChecks(w http.ResponseWriter, r *http.Request) {
	appSlug := mux.Vars(r)["appSlug"]
	sequence, err := strconv.Atoi(mux.Vars(r)["sequence"])
	if err != nil {
		logger.Error(err)
		w.WriteHeader(400)
		return
	}

	foundApp, err := store.GetStore().GetAppFromSlug(appSlug)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	if err := store.GetStore().ResetPreflightResults(foundApp.ID, int64(sequence)); err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	archiveDir, err := ioutil.TempDir("", "kotsadm")
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	removeArchiveDir := true
	defer func() {
		if removeArchiveDir {
			os.RemoveAll(archiveDir)
		}
	}()

	err = store.GetStore().GetAppVersionArchive(foundApp.ID, int64(sequence), archiveDir)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	removeArchiveDir = false
	go func() {
		defer os.RemoveAll(archiveDir)
		if err := preflight.Run(foundApp.ID, foundApp.Slug, int64(sequence), foundApp.IsAirgap, archiveDir); err != nil {
			logger.Error(err)
			return
		}
	}()

	JSON(w, 200, struct{}{})
}

func (h *Handler) GetPreflightCommand(w http.ResponseWriter, r *http.Request) {
	appSlug := mux.Vars(r)["appSlug"]
	sequence, err := strconv.ParseInt(mux.Vars(r)["sequence"], 10, 64)
	if err != nil {
		logger.Error(errors.Wrap(err, "failed to parse sequence"))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	inCluster := r.URL.Query().Get("inCluster") == "true"

	getPreflightCommandRequest := GetPreflightCommandRequest{}
	if err := json.NewDecoder(r.Body).Decode(&getPreflightCommandRequest); err != nil {
		logger.Error(errors.Wrap(err, "failed to decode request body"))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	foundApp, err := store.GetStore().GetAppFromSlug(appSlug)
	if err != nil {
		logger.Error(errors.Wrap(err, "failed to get app"))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	archivePath, err := ioutil.TempDir("", "kotsadm")
	if err != nil {
		logger.Error(errors.Wrap(err, "failed to create temp dir"))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(archivePath)

	err = store.GetStore().GetAppVersionArchive(foundApp.ID, sequence, archivePath)
	if err != nil {
		logger.Error(errors.Wrap(err, "failed to get app archive"))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	kotsKinds, err := kotsutil.LoadKotsKindsFromPath(archivePath)
	if err != nil {
		logger.Error(errors.Wrap(err, "failed to load kots kinds"))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = preflight.CreateRenderedSpec(foundApp.ID, sequence, getPreflightCommandRequest.Origin, inCluster, kotsKinds.Preflight)
	if err != nil {
		logger.Error(errors.Wrap(err, "failed to render preflight spec"))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	response := GetPreflightCommandResponse{
		Command: preflight.GetPreflightCommand(foundApp.Slug),
	}

	JSON(w, http.StatusOK, response)
}

// PostPreflightStatus route is UNAUTHENTICATED
// This request comes from the `kubectl preflight` command.
func (h *Handler) PostPreflightStatus(w http.ResponseWriter, r *http.Request) {
	appSlug := mux.Vars(r)["appSlug"]
	sequence, err := strconv.ParseInt(mux.Vars(r)["sequence"], 10, 64)
	if err != nil {
		err = errors.Wrap(err, "failed to parse sequence")
		logger.Error(err)
		w.WriteHeader(400)
		return
	}

	foundApp, err := store.GetStore().GetAppFromSlug(appSlug)
	if err != nil {
		err = errors.Wrap(err, "failed to get app from slug")
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		err = errors.Wrap(err, "failed to read request body")
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	if err := store.GetStore().SetPreflightResults(foundApp.ID, sequence, b); err != nil {
		err = errors.Wrap(err, "failed to set preflight results")
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	w.WriteHeader(204)
}

func (h *Handler) SkipPreflights(w http.ResponseWriter, r *http.Request) {
	appSlug := mux.Vars(r)["appSlug"]

	foundApp, err := store.GetStore().GetAppFromSlug(appSlug)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	license, err := store.GetStore().GetLatestLicenseForApp(foundApp.ID)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	downstreams, err := store.GetStore().ListDownstreamsForApp(foundApp.ID)
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

	clusterID := downstreams[0].ClusterID

	if err := sendPreflightsReportToReplicatedApp(license, appSlug, clusterID, 0, true, ""); err != nil {
		err = errors.Wrap(err, "failed to send preflights data to replicated app")
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	JSON(w, 200, struct{}{})
}

func sendPreflightsReportToReplicatedApp(license *kotsv1beta1.License, appSlug string, clusterID string, sequence int, skipPreflights bool, installStatus string) error {
	urlValues := url.Values{}

	sequenceToStr := fmt.Sprintf("%d", sequence)
	skipPreflightsToStr := fmt.Sprintf("%t", skipPreflights)

	urlValues.Set("sequence", sequenceToStr)
	urlValues.Set("skipPreflights", skipPreflightsToStr)
	urlValues.Set("installStatus", installStatus)

	url := fmt.Sprintf("%s/preflights/reporting/%s/%s?%s", license.Spec.Endpoint, appSlug, clusterID, urlValues.Encode())

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
