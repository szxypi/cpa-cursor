package main

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName    = "cpa-cursor"
	pluginVersion = "0.3.32"
	providerKey   = "cursor"
	logPrefix     = "[cpa-cursor] "
)

func main() { fmt.Println(pluginName, pluginVersion) }

// pluginConfig is the plugins.configs.cpa-cursor block in config.yaml.
type pluginConfig struct {
	Enabled               bool   `yaml:"enabled"`
	AgentDiagnostics      bool   `yaml:"agent_diagnostics"`
	AgentDiagnosticsModel string `yaml:"agent_diagnostics_model"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{Enabled: true}
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func configure(raw []byte) error {
	cfg := defaultConfig()
	if len(raw) > 0 {
		var req lifecycleRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
		if len(req.ConfigYAML) > 0 {
			if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
				return err
			}
		}
	}
	setAgentDiagnostics(cfg.AgentDiagnostics, cfg.AgentDiagnosticsModel)
	return nil
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	CommandLinePlugin     bool                         `json:"command_line_plugin"`
	ManagementAPI         bool                         `json:"management_api"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "szxypi",
			GitHubRepository: "szxypi/cpa-cursor",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "enabled",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "是否启用 cursor 提供方（导入 Cursor IDE 的 token 后即可路由）",
				},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider: true,
			AuthProvider:  true,
			Executor:      true,
			// Every model comes from the imported token's account catalog.
			ExecutorModelScope: pluginapi.ExecutorModelScopeOAuth,
			// Cursor's protocol is translated to/from OpenAI chat chunks
			// inside the plugin; CPA feeds it the openai shape.
			ExecutorInputFormats:  []string{"openai"},
			ExecutorOutputFormats: []string{"openai"},
			CommandLinePlugin:     true,
			ManagementAPI:         true,
		},
	}
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		agentCheckpoints.enableDisk()
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())

	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": providerKey})

	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(request)

	case pluginabi.MethodModelStatic:
		return handleModelStatic()
	case pluginabi.MethodModelForAuth:
		return handleAuthModels(request)

	case pluginabi.MethodExecutorExecute:
		return handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecutorExecuteStream(request)

	case pluginabi.MethodCommandLineRegister:
		return handleCommandLineRegister(request)
	case pluginabi.MethodCommandLineExecute:
		return handleCommandLineExecute(request)

	case pluginabi.MethodManagementRegister:
		return handleManagementRegister()
	case pluginabi.MethodManagementHandle:
		return handleManagementRequest(request)

	case pluginabi.MethodPluginShutdown:
		agentCheckpoints.reset()
		return okEnvelope(map[string]any{})
	case pluginabi.MethodPluginQuiesce:
		return okEnvelope(map[string]any{})

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// shutdownEngine is invoked from the C ABI shutdown hook.
func shutdownEngine() { agentCheckpoints.reset() }

// handleModelStatic reports no models: every cursor model is account-scoped
// and arrives through model.for_auth.
func handleModelStatic() ([]byte, error) {
	return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
}
