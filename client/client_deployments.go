package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mendersoftware/log"
	"github.com/pkg/errors"
)

const (
	// StatusInstalling is used whenever the device is
	// transferring data to storage
	StatusInstalling Status = "installing"
	// StatusDownloading is reported prior to start downloading the update
	StatusDownloading = "downloading"
	// StatusRebooting is reported prior to rebooting
	StatusRebooting = "rebooting"
	// StatusSuccess is reported when the device has confirmed that the
	// update was installed successfully.
	StatusSuccess = "success"
	// StatusFailure is reported whenever the update process fails
	StatusFailure = "failure"
	// StatusAlreadyInstalled is reported if the update is already installed
	// on the device
	StatusAlreadyInstalled = "already-installed"
)

var (
	// ErrAlreadyInstalled is returned by the CheckUpdate if the artifact
	// is already installed
	ErrAlreadyInstalled = fmt.Errorf("artifact already installed")

	ErrURLExpired = fmt.Errorf("url expired")
)

// LogMessage resembles the JSON-structure of a log request.
type LogMessage struct {
	Level     string `json:"level"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// Status is used to fix the number of valid statuses (see constants below)
type Status string

// DeploymentStatus resembles the JSON-structure of a status request with the
// addition of the ID of the target deployment.
type DeploymentStatus struct {
	DeploymentID string `json:"-"`
	Status       Status `json:"status"`
	SubState     string `json:"substate,omitempty"`
}

// DeploymentInstructions resembles the JSON response of a CheckUpdate request.
type DeploymentInstructions struct {
	// ID of the pending deployment.
	DeploymentID string `json:"id"`
	// Artifact describes the target content.
	Artifact struct {
		ArtifactName string   `json:"artifact_name"`
		DeviceTypes  []string `json:"device_types_compatible"`
		// Source Describes where to fetch the artifact.
		Source struct {
			Expire string `json:"expire,omitempty"`
			URL    string `json:"url,omitempty"`
		} `json:"source"`
	} `json:"artifact"`
}

// Validate checks that conditions are satisfied for the deployment.
func (di *DeploymentInstructions) Validate(updateInfo UpdateInfo) error {

	var deviceTypeOK bool = false

	for _, dt := range di.Artifact.DeviceTypes {
		if strings.Compare(updateInfo.DeviceType, dt) == 0 {
			deviceTypeOK = true
			break
		}
	}
	if !deviceTypeOK {
		return fmt.Errorf("invalid deployment instructions: "+
			"device type not satisfied: %s not in %v",
			updateInfo.DeviceType, di.Artifact.DeviceTypes)
	}

	if strings.Compare(updateInfo.ArtifactName,
		di.Artifact.ArtifactName) == 0 {
		return ErrAlreadyInstalled
	}
	return nil
}

func (client *MenderClient) buildUpdateRequest() (*http.Request, error) {
	var queryParams url.Values
	serverURL := client.servers[client.activeServer].ServerURL
	queryParams.Add("artifact_name", client.updateInfo.ArtifactName)
	queryParams.Add("device_type", client.updateInfo.DeviceType)
	serverURL += ApiDeploymentsNext + "?" + queryParams.Encode()

	req, err := http.NewRequest("GET", serverURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Authorization",
		"Bearer "+string(client.servers[client.activeServer].APIToken))
	return req, nil
}

// CheckUpdate checks if there are any pending deployments.
func (client *MenderClient) CheckUpdate() (*DeploymentInstructions, error) {
	req, err := client.buildUpdateRequest()
	if err != nil {
		return nil, errors.Wrap(err, "failed to build update request")
	}
	defer req.Body.Close()

	rsp, err := client.httpClient.Do(req)
	if err != nil {
		log.Error("Checking for update failed: %v", err)
		return nil, errors.Wrap(err, "failed to check for update")
	}
	switch rsp.StatusCode {
	case http.StatusNoContent:
		// No update scheduled
		return nil, nil
	case http.StatusOK:
		// New update ready
		body, err := ioutil.ReadAll(rsp.Body)
		if err != nil {
			return nil, errors.Wrap(err,
				"error reading HTTP content")
		}

		instructions := new(DeploymentInstructions)
		err = json.Unmarshal(body, instructions)
		if err != nil {
			return nil, errors.Wrap(err,
				"error parsing HTTP content")
		}
		err = instructions.Validate(client.updateInfo)
		if err == ErrAlreadyInstalled {
			errStatus := client.UpdateStatus(DeploymentStatus{
				DeploymentID: instructions.DeploymentID,
				Status:       StatusAlreadyInstalled,
			})
			if errStatus != nil {
				return nil, errors.Wrapf(err,
					"failed to update status: %s",
					errStatus.Error())
			}
			return nil, err
		} else if err != nil {
			return nil, err
		}
		return instructions, nil

	case http.StatusBadRequest:
		return nil, NewAPIError(fmt.Errorf("400 malformed request"), rsp)
	}
	return nil, NewAPIError(
		fmt.Errorf("Bad request (status: %s)", rsp.Status), rsp)
}

func (client *MenderClient) FetchUpdate(
	instr *DeploymentInstructions) (io.ReadCloser, error) {
	if instr.Artifact.Source.Expire != "" {
		expire, err := time.Parse(
			time.RFC3339, instr.Artifact.Source.Expire)
		if err != nil {
			log.Warn("Unable to check update link expiry date: " +
				err.Error())
		}
		if expire.Before(time.Now()) {
			log.Errorf("The update URL [1] is already expired "+
				"[1: %s]", instr.Artifact.Source.URL)
			return nil, ErrURLExpired
		}
	}

	if instr.Artifact.Source.URL == "" {
		// This is not an error according to the API specs
		log.Info("Update instructions does not contain a URL.")
		return nil, nil
	}
	req, err := http.NewRequest("GET", instr.Artifact.Source.URL, nil)
	if err != nil {
		log.Error("Error building update fetch request")
		return nil, errors.Wrap(err, "error building update request")
	}

	rsp, err := client.httpClient.Do(req)
	if err != nil {
		log.Error("Error fetching update: " + err.Error())
		return nil, errors.Wrap(err, "error fetching update")
	}

	resumeFunc := func(offset int64) (io.ReadCloser, int64, int64, error) {
		// Make a new request with the Range header set to the current
		// offset, and parse / verify the received Content-Range header.
		// The function returns a new request body, a new offset and
		// content length on success, or emits an error on failure.
		var first, last, size int64
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		rsp, err := client.httpClient.Do(req)
		if err != nil {
			return nil, 0, 0, err
		}

		if offset > 0 && rsp.StatusCode != http.StatusPartialContent {
			rsp.Body.Close()
			return nil, 0, 0, fmt.Errorf("Could not resume "+
				"download from offset %d. HTTP status code: %s",
				offset, rsp.Status)
		}
		hRangeStr := rsp.Header.Get("Content-Range")
		log.Debugf("Content-Range received from server: '%s'", hRangeStr)

		_, err = fmt.Sscanf(strings.TrimSpace(hRangeStr),
			"bytes%d-%d/%d", &first, &last, &size)
		if err != nil || size <= 0 || (last+1) != size || last < first {
			rsp.Body.Close()
			return nil, 0, 0, fmt.Errorf("malformed Content-Range "+
				"received from server '%s'", hRangeStr)
		}
		return rsp.Body, first, size, nil
	}

	return NewUpdateResumer(
		rsp.Body, rsp.ContentLength, time.Minute, resumeFunc), nil
}

func (client *MenderClient) buildLogRequest(
	deploymentID string, logs []LogMessage) (*http.Request, error) {

	apiToken := client.servers[client.activeServer].APIToken
	if apiToken == nil {
		_, err := client.Authorize()
		if err != nil {
			return nil, ErrDeviceNotAuthorized
		}
		apiToken = client.servers[client.activeServer].APIToken
	}
	body, err := json.Marshal(logs)
	if err != nil {
		log.Error("error serializing log request body")
		return nil, errors.Wrap(err,
			"error serializing log request body")
	}
	serverURL := client.servers[client.activeServer].ServerURL
	serverURL += fmt.Sprintf(ApiDeploymentsLog, deploymentID)

	req, err := http.NewRequest("PUT", serverURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(apiToken))

	return req, nil
}

// Log makes a deployment log request to the backend.
func (client *MenderClient) Log(deploymentID string, logs []LogMessage) error {
	req, err := client.buildLogRequest(deploymentID, logs)

	if err != nil {
		return errors.Wrap(err, "error building log request")
	}

	rsp, err := client.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to make log request")
	}
	switch rsp.StatusCode {
	case http.StatusNoContent:
		return nil
	}

	return NewAPIError(
		fmt.Errorf("bad http response (status: %v)", rsp.Status), rsp)
}

func (client *MenderClient) buildStatusRequest(status DeploymentStatus) (*http.Request, error) {
	url := strings.TrimSuffix(client.servers[0].ServerURL, "/")
	path := fmt.Sprintf(ApiDeploymentsStatus, status.DeploymentID)

	body, err := json.Marshal(status)
	if err != nil {
		return nil, errors.Wrapf(err,
			"failed to serialize status request body")
	}

	hreq, err := http.NewRequest(http.MethodPut, url+path, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization",
		"Bearer "+string(client.servers[client.activeServer].APIToken))
	return hreq, nil
}

// UpdateStatus updates the status of the requested deployment to the given value
func (client *MenderClient) UpdateStatus(status DeploymentStatus) error {
	req, err := client.buildStatusRequest(status)
	if err != nil {
		return errors.Wrapf(err, "failed to prepare status report request")
	}

	r, err := client.Do(req)
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
