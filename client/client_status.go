// Copyright 2019 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/mendersoftware/log"
	"github.com/pkg/errors"
)

const (
	StatusInstalling       = "installing"
	StatusDownloading      = "downloading"
	StatusRebooting        = "rebooting"
	StatusSuccess          = "success"
	StatusFailure          = "failure"
	StatusAlreadyInstalled = "already-installed"
)

var (
	ErrDeploymentAborted = errors.New("deployment was aborted")
)

type StatusReporter interface {
	Report(api ApiRequester, server string, report StatusReport) error
}

type StatusReport struct {
	DeploymentID string `json:"-"`
	Status       string `json:"status"`
	SubState     string `json:"substate,omitempty"`
}

// StatusReportWrapper holds the data that is passed to the
// statescript functions upon reporting script exectution-status
// to the backend.
type StatusReportWrapper struct {
	Report StatusReport
	API    ApiRequester
	URL    string
}

type StatusClient struct {
}

func NewStatus() StatusReporter {
	return &StatusClient{}
}

// Report status information to the backend
func (u *StatusClient) Report(api ApiRequester, url string, report StatusReport) error {
	req, err := makeStatusReportRequest(url, report)
	if err != nil {
		return errors.Wrapf(err, "failed to prepare status report request")
	}

	r, err := api.Do(req)
	if err != nil {
		log.Error("failed to report status: ", err)
		return errors.Wrapf(err, "reporting status failed")
	}

	defer r.Body.Close()

	// HTTP 204 No Content
	switch {
	case r.StatusCode == http.StatusConflict:
		log.Warnf("status report rejected, deployment aborted at the backend")
		return NewAPIError(ErrDeploymentAborted, r)
	case r.StatusCode != http.StatusNoContent:
		log.Errorf("got unexpected HTTP status when reporting status: %v", r.StatusCode)
		return NewAPIError(errors.Errorf("reporting status failed, bad status %v", r.StatusCode), r)
	}

	log.Debugf("status reported, response %s", r.Status)

	return nil
}

func makeStatusReportRequest(server string, report StatusReport) (*http.Request, error) {
	path := fmt.Sprintf("/deployments/device/deployments/%s/status",
		report.DeploymentID)
	url := buildApiURL(server, path)

	out := &bytes.Buffer{}
	enc := json.NewEncoder(out)
	err := enc.Encode(&report)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to encode status request data")
	}

	hreq, err := http.NewRequest(http.MethodPut, url, out)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create status HTTP request")
	}

	hreq.Header.Add("Content-Type", "application/json")
	return hreq, nil
}

// progressReporter contains a sub-routine that reports the progress of the
// state (be it Downloading or Installing).
func (u *StatusClient) progressReporter(ctx context.Context, pollInterval time.Duration,
	url string, status StatusReport, progressPtr interface{}, limit int64) {
	var lastProgress int64
	var progress int64
	// FIXME
	_, err := makeStatusReportRequest(url, status)
	if err != nil {
		return
	}
	// ${1} - preamble
	// ${2} - "$progress="
	// ${3} = {progress}(%)
	// ${4} - decimal
	// ${5} - epilogue
	progressRe, err := regexp.Compile(`(.*?)($progress=)([0-9]+(.[0-9]+)?)?(.*)`)
	if err != nil {
		return
	}

	dereferenceInt64 := func(i interface{}) int64 {
		// Returns non-negative integer value or negative value
		// if type is incompatible.
		var ret int64
		switch i.(type) {
		case *int64:
			ret = *i.(*int64)
		case *uint64:
			ret = int64(*i.(*uint64))
		case *int32:
			ret = int64(*i.(*int32))
		case *uint32:
			ret = int64(*i.(*uint32))
		case *int:
			ret = int64(*i.(*int))
		case *uint:
			ret = int64(*i.(*uint))

		default:
			return -1
		}
		return ret
	}

	// Check if pointers has an integer type of significant width
	if dereferenceInt64(limit) < 0 {
		return
	}
	if progress = dereferenceInt64(progressPtr); lastProgress < 0 {
		return
	}

	if !progressRe.MatchString(status.SubState) {
		status.SubState = "$progress=0," + status.SubState
	}

	for {
		if lastProgress != progress {
			lastProgress = progress
			pf := (float64(progress) / float64(limit)) * 100.0
			status.SubState = progressRe.ReplaceAllString(
				status.SubState,
				fmt.Sprintf(`${1}${2}%.2f${5}`, pf),
			)
		}
		lastProgress = progress
		if progress >= limit {
			break
		}
		select {
		case <-time.After(pollInterval):

		case <-ctx.Done():
			return
		}

		progress = dereferenceInt64(progressPtr)
	}
}
