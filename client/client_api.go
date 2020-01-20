// Copyright 2020 Northern.tech AS
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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Api path definitions
const (
	// Deployments
	ApiDeploymentsNext   = "/api/v1/deployments/device/deployments/next"
	ApiDeploymentsLog    = "/api/v1/deployments/device/deployments/%s/log"
	ApiDeploymentsStatus = "/api/v1/deployments/device/deployments/%s/status"

	// Inventory
	ApiInventory = "/api/devices/v1/inventory"

	// Authentication
	ApiAuthPath = "/api/devices/v1/authentication"
)

var (
	ErrDeploymentAborted   = fmt.Errorf("deployment was aborted")
	ErrDeviceNotAuthorized = fmt.Errorf("device not authorized")
)

// ApiRequester is a http.Client compliant interface. A standard http.Client is
// compatible with this interface and can be used without further configuration
// where ApiRequester is expected. Instead of instantiating the client by
// yourself, one can also use a wrapper call NewApiClient() that sets up TLS
// handling according to passed configuration.
type APIRequester interface {
	Do(req *http.Request) (*http.Response, error)
}

// APIError is an error type returned after receiving an error message from the
// server. It wraps a regular error with the request_id - and if
// the server returns an error message, this is also returned.
type APIError struct {
	error
	reqID        string
	serverErrMsg string
}

func NewAPIError(err error, resp *http.Response) *APIError {
	a := APIError{
		error: err,
		reqID: resp.Header.Get("request_id"),
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 600 {
		a.serverErrMsg = unmarshalErrorMessage(resp.Body)
	}
	return &a
}

func (a *APIError) Error() string {

	err := fmt.Sprintf("(request_id: %s): %s", a.reqID, a.error.Error())

	if a.serverErrMsg != "" {
		return err + fmt.Sprintf(" server error message: %s", a.serverErrMsg)
	}

	return err

}

// Cause returns the underlying error, as
// an APIError is merely an error wrapper.
func (a *APIError) Cause() error {
	return a.error
}

// unmarshalErrorMessage unmarshals the error message contained in an
// error request from the server.
func unmarshalErrorMessage(r io.Reader) string {
	e := new(struct {
		Error string `json:"error"`
	})
	if err := json.NewDecoder(r).Decode(e); err != nil {
		return fmt.Sprintf("failed to parse server response: %v", err)
	}
	return e.Error
}
