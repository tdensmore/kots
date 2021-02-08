package reporting

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/kotsadm/pkg/downstream"
	"github.com/replicatedhq/kots/kotsadm/pkg/logger"
	"github.com/replicatedhq/kots/kotsadm/pkg/store"
	kotsv1beta1 "github.com/replicatedhq/kots/kotskinds/apis/kots/v1beta1"
)

func SendPreflightsReportToReplicatedApp(license *kotsv1beta1.License, appSlug string, clusterID string, sequence int, skipPreflights bool, installStatus string) error {
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

func PreflightInfoThreadSend(appID string, appSlug string, sequence int, isSkipPreflights bool, isFailedPreflight bool) error {
	go func() {
		<-time.After(20 * time.Second)
		license, err := store.GetStore().GetLatestLicenseForApp(appID)
		if err != nil {
			logger.Error(errors.Wrap(err, "failed to find license for app"))
			return
		}

		downstreams, err := store.GetStore().ListDownstreamsForApp(appID)
		if err != nil {
			logger.Error(errors.Wrap(err, "failed to list downstreams for app"))
			return
		} else if len(downstreams) == 0 {
			logger.Error(errors.Wrap(err, "no downstreams for app"))
			return
		}

		clusterID := downstreams[0].ClusterID

		if isSkipPreflights || isFailedPreflight {
			currentVersion, err := downstream.GetCurrentVersion(appID, clusterID)
			if err != nil {
				logger.Debugf("failed to get current downstream version", err)
				return
			}

			if err := SendPreflightsReportToReplicatedApp(license, appSlug, clusterID, sequence, isSkipPreflights, currentVersion.Status); err != nil {
				logger.Error(errors.Wrap(err, "failed to send preflights data to replicated app"))
				return
			}
		} else {
			if err := SendPreflightsReportToReplicatedApp(license, appSlug, clusterID, sequence, isSkipPreflights, ""); err != nil {
				logger.Error(errors.Wrap(err, "failed to send preflights data to replicated app"))
				return
			}
		}
	}()

	return nil
}
