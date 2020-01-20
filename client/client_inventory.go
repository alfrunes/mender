package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/pkg/errors"
)

type InventoryItem struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

func (client *MenderClient) buildInventoryRequest(
	items []InventoryItem) (*http.Request, error) {
	var req *http.Request
	apiToken := client.servers[client.activeServer].APIToken
	if apiToken == nil {
		return nil, ErrDeviceNotAuthorized
	}

	body, err := json.Marshal(items)
	if err != nil {
		return nil, errors.Wrap(err,
			"error serializing inventory request body")
	}
	url := client.servers[client.activeServer].ServerURL
	req, err = http.NewRequest("PATCH", url+ApiInventory, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization",
		"Bearer "+string(apiToken))

	return req, nil
}

func (client *MenderClient) UpdateInventory(items []InventoryItem) error {
	req, err := client.buildInventoryRequest(items)
	if err != nil {
		return err
	}
	rsp, err := client.httpClient.Do(req)
	if err != nil {
		return err
	}
	switch rsp.StatusCode {
	case http.StatusBadRequest:
		return NewAPIError(fmt.Errorf("400 Bad request"), rsp)
	case http.StatusInternalServerError:
		return NewAPIError(fmt.Errorf("500 Internal server error"), rsp)
	case http.StatusOK:
		return nil
	}
	return NewAPIError(fmt.Errorf("Bad status: %v", rsp.Status), rsp)
}
