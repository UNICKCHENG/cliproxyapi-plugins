package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// optimizeForParameter is the model parameter the Cursor Router reads. The router model rejects a
// request that omits it, so it is the one parameter the plugin backfills.
const optimizeForParameter = "optimize_for"

// catalogTTL bounds how long a credential's model catalog is reused. Which models a key can
// reach follows the account plan and team policy, both of which change outside this process.
const catalogTTL = 5 * time.Minute

// catalogTimeout bounds a catalog fetch. Discovery is advisory on the generation path, so a slow
// catalog must not hold a run hostage.
const catalogTimeout = 30 * time.Second

// modelCatalogs caches the catalog per credential, keyed by a digest of the API key so the key
// itself is never a map key that could reach a log or a panic dump.
var modelCatalogs sync.Map

type catalogEntry struct {
	models  []*sdkv1.SdkModel
	fetched time.Time
}

func catalogCacheKey(apiKey string) string {
	digest := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(digest[:])
}

func evictStaleCatalogs() {
	now := time.Now()
	modelCatalogs.Range(func(key, value any) bool {
		entry, ok := value.(catalogEntry)
		if !ok || now.Sub(entry.fetched) > catalogTTL {
			modelCatalogs.Delete(key)
		}
		return true
	})
}

// catalogFor returns the models visible to one credential, fetching them when the cache is cold
// or stale. A refresh forces a fetch regardless of age.
func catalogFor(ctx context.Context, process *bridgeProcess, apiKey string, refresh bool) ([]*sdkv1.SdkModel, error) {
	key := catalogCacheKey(apiKey)
	if !refresh {
		if cached, ok := modelCatalogs.Load(key); ok {
			entry := cached.(catalogEntry)
			if time.Since(entry.fetched) < catalogTTL {
				return entry.models, nil
			}
			modelCatalogs.Delete(key)
		}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	response, errList := process.cursor.ListModels(fetchCtx, connect.NewRequest(&sdkv1.ListModelsRequest{
		Options: &sdkv1.CursorRequestOptions{ApiKey: apiKey},
	}))
	if errList != nil {
		return nil, errList
	}
	models := response.Msg.GetItems()
	modelCatalogs.Store(key, catalogEntry{models: models, fetched: time.Now()})
	evictStaleCatalogs()
	return models, nil
}

// resolveModelSelection builds a ModelSelection the bridge will accept.
//
// Parameters the selected model does not expose are dropped rather than forwarded, because the
// upstream rejects an unknown parameter outright, and the router's optimize_for is filled in from
// configuration when the caller omitted it.
//
// Matching is by model id only. The TypeScript SDK's catalog carried an aliases list that this
// resolution used to accept as well; sdk.v1's SdkModel has no such field, so an alias now has to
// come from the host's own oauth-model-alias mapping.
func resolveModelSelection(models []*sdkv1.SdkModel, modelID string, requested []modelParam, optimizeFor string) *sdkv1.ModelSelection {
	entry := findCatalogModel(models, modelID)
	if entry == nil {
		// Unknown to the catalog, or the catalog was unavailable: forward the request as it came
		// and let Cursor decide.
		return &sdkv1.ModelSelection{Id: modelID, Params: modelParamValues(requested)}
	}
	params := make([]*sdkv1.ModelParameterValue, 0, len(entry.GetParameters()))
	for _, definition := range entry.GetParameters() {
		allowed := allowedParameterValues(definition)
		if supplied, ok := suppliedParameter(requested, definition.GetId()); ok && allowed[supplied] {
			params = append(params, &sdkv1.ModelParameterValue{Id: definition.GetId(), Value: supplied})
			continue
		}
		if definition.GetId() != optimizeForParameter {
			continue
		}
		if preferred := preferredOptimizeFor(definition, optimizeFor); preferred != "" {
			params = append(params, &sdkv1.ModelParameterValue{Id: definition.GetId(), Value: preferred})
		}
	}
	selection := &sdkv1.ModelSelection{Id: entry.GetId()}
	if len(params) > 0 {
		selection.Params = params
	}
	return selection
}

func findCatalogModel(models []*sdkv1.SdkModel, modelID string) *sdkv1.SdkModel {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	for _, model := range models {
		if model.GetId() == modelID {
			return model
		}
	}
	return nil
}

func allowedParameterValues(definition *sdkv1.ModelParameterDefinition) map[string]bool {
	allowed := make(map[string]bool, len(definition.GetValues()))
	for _, value := range definition.GetValues() {
		allowed[value.GetValue()] = true
	}
	return allowed
}

func suppliedParameter(requested []modelParam, id string) (string, bool) {
	for _, param := range requested {
		if param.ID == id {
			return param.Value, true
		}
	}
	return "", false
}

// preferredOptimizeFor picks the configured mode when the model offers it, otherwise the first
// value it does offer, so the router always receives something valid.
func preferredOptimizeFor(definition *sdkv1.ModelParameterDefinition, optimizeFor string) string {
	values := definition.GetValues()
	for _, value := range values {
		if value.GetValue() == optimizeFor {
			return optimizeFor
		}
	}
	if len(values) > 0 {
		return values[0].GetValue()
	}
	return ""
}

func modelParamValues(params []modelParam) []*sdkv1.ModelParameterValue {
	if len(params) == 0 {
		return nil
	}
	values := make([]*sdkv1.ModelParameterValue, 0, len(params))
	for _, param := range params {
		values = append(values, &sdkv1.ModelParameterValue{Id: param.ID, Value: param.Value})
	}
	return values
}
