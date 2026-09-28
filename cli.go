package main

// Command-line import: --cursor-login <accessToken> [--cursor-machine-id <id>]
// [--cursor-email <email>]. There is no OAuth dance to run server-side — the
// token comes from a logged-in Cursor IDE (state.vscdb "cursorAuth/accessToken"
// or an existing 9router credential), so importing is all this plugin can do.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func handleCommandLineRegister(request []byte) ([]byte, error) {
	return okEnvelope(pluginapi.CommandLineRegistrationResponse{
		Flags: []pluginapi.CommandLineFlag{
			{
				Name:         "cursor-login",
				Usage:        "import a Cursor access token captured from a logged-in Cursor IDE (state.vscdb)",
				Type:         "string",
				DefaultValue: "",
			},
			{
				Name:         "cursor-machine-id",
				Usage:        "machine id paired with --cursor-login (optional; derived from the token when omitted)",
				Type:         "string",
				DefaultValue: "",
			},
			{
				Name:         "cursor-email",
				Usage:        "account email recorded on the imported credential (optional)",
				Type:         "string",
				DefaultValue: "",
			},
		},
	})
}

func handleCommandLineExecute(request []byte) ([]byte, error) {
	var req pluginapi.CommandLineExecutionRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}

	tokenFlag := req.TriggeredFlags["cursor-login"]
	if !tokenFlag.Set || strings.TrimSpace(tokenFlag.Value) == "" {
		return errorEnvelopeStatus("invalid_request",
			"--cursor-login needs the access token as its value", 2), nil
	}

	cred := &cursorCredential{
		AccessToken: strings.TrimSpace(tokenFlag.Value),
	}
	if machineFlag, ok := req.TriggeredFlags["cursor-machine-id"]; ok && strings.TrimSpace(machineFlag.Value) != "" {
		cred.MachineID = strings.TrimSpace(machineFlag.Value)
	}
	if emailFlag, ok := req.TriggeredFlags["cursor-email"]; ok && strings.TrimSpace(emailFlag.Value) != "" {
		cred.Email = strings.TrimSpace(emailFlag.Value)
	}
	if _, err := parseCursorCredential(cred.storageJSON()); err != nil {
		return errorEnvelopeStatus("invalid_credential", err.Error(), 2), nil
	}

	message := fmt.Sprintf("imported cursor credential %s (file %s)\n", cred.label(), cred.fileName())
	return okEnvelope(pluginapi.CommandLineExecutionResponse{
		Stdout: []byte(message),
		Auths: []pluginapi.AuthData{
			{
				Provider:    providerKey,
				FileName:    cred.fileName(),
				Label:       cred.label(),
				StorageJSON: cred.storageJSON(),
				Metadata:    cred.metadata(),
			},
		},
	})
}
