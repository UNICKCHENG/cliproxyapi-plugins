package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	quotaObservationTimeout = 30 * time.Second
	quotaResponseMaxBytes   = 1 << 20
	quotaDashboardPath      = "/aiserver.v1.DashboardService/"
)

var quotaTransportFactory = func(proxy *url.URL) http.RoundTripper { return egressTransport(proxy) }

type quotaFetchRequest struct {
	AuthIndex   string `json:"auth_index"`
	AuthID      string `json:"auth_id"`
	Provider    string `json:"provider"`
	StorageJSON []byte `json:"storage_json,omitempty"`
}

type includedUsage struct {
	autoUsed    float64
	apiUsed     float64
	periodStart int64
	periodEnd   int64
}

type quotaResponse struct {
	Groups []quotaGroup `json:"groups"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []quotaBucket `json:"buckets"`
}

type quotaBucket struct {
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime"`
}

func fetchCursorQuota(raw []byte) ([]byte, error) {
	var req quotaFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil || strings.TrimSpace(req.AuthIndex) == "" ||
		strings.TrimSpace(req.AuthID) == "" || req.Provider != providerIdentifier {
		return errorEnvelope("invalid_quota_request", "cursor quota requires indexed cursor credential context"), nil
	}
	key, err := requireAPIKey(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_auth", "cursor quota requires a selected API key"), nil
	}
	target := cursorBackendTarget
	proxy := resolveEgressProxy(resolveProxyURL(req.StorageJSON))
	var parsedProxy *url.URL
	if proxy != "" {
		parsedProxy, err = parseEgressProxy(proxy)
		if err != nil {
			return errorEnvelope("quota_transport", "cursor quota proxy configuration is invalid"), nil
		}
	} else if backend := os.Getenv("CURSOR_BACKEND_URL"); backend != "" && backend != target.String() && backend != target.String()+"/" {
		return errorEnvelope("quota_transport", "cursor quota backend override is not supported"), nil
	}
	transport := quotaTransportFactory(parsedProxy)
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(context.Background(), quotaObservationTimeout)
	defer cancel()
	id := registerInFlight(cancel)
	defer unregisterInFlight(id)
	usage, err := observeCursorQuota(ctx, client, target, key)
	if err != nil {
		return errorEnvelope("quota_unavailable", err.Error()), nil
	}
	reset := time.UnixMilli(usage.periodEnd).UTC().Format(time.RFC3339Nano)
	return okEnvelope(quotaResponse{Groups: []quotaGroup{
		{DisplayName: "Auto", Buckets: []quotaBucket{{RemainingFraction: (100 - usage.autoUsed) / 100, ResetTime: reset}}},
		{DisplayName: "API", Buckets: []quotaBucket{{RemainingFraction: (100 - usage.apiUsed) / 100, ResetTime: reset}}},
	}})
}

func observeCursorQuota(ctx context.Context, client *http.Client, target url.URL, key string) (includedUsage, error) {
	exchange, err := quotaPost(ctx, client, target, "/auth/exchange_user_api_key", key, false)
	if err != nil {
		return includedUsage{}, errors.New("cursor quota exchange failed")
	}
	fields, err := quotaObject(exchange, "accessToken")
	var token string
	if err != nil || json.Unmarshal(fields["accessToken"], &token) != nil || strings.TrimSpace(token) == "" {
		return includedUsage{}, errors.New("cursor quota exchange response is invalid")
	}
	me, err := quotaPost(ctx, client, target, quotaDashboardPath+"GetMe", token, true)
	if err != nil {
		return includedUsage{}, errors.New("cursor quota identity read failed")
	}
	if err := validateQuotaIdentity(me); err != nil {
		return includedUsage{}, errors.New("cursor quota identity response is invalid")
	}
	rawUsage, err := quotaPost(ctx, client, target, quotaDashboardPath+"GetCurrentPeriodUsage", token, true)
	if err != nil {
		return includedUsage{}, errors.New("cursor quota usage read failed")
	}
	usage, err := parseCursorUsage(rawUsage)
	if err != nil {
		return includedUsage{}, errors.New("cursor quota usage response is invalid")
	}
	hardLimit, err := quotaPost(ctx, client, target, quotaDashboardPath+"GetHardLimit", token, true)
	if err != nil {
		return includedUsage{}, errors.New("cursor quota limit read failed")
	}
	if err := validateQuotaHardLimit(hardLimit); err != nil {
		return includedUsage{}, errors.New("cursor quota limit response is invalid")
	}
	// The local completion clock is a conservative display check, not a provider clock.
	now := time.Now().UnixMilli()
	if ctx.Err() != nil || now < usage.periodStart || now >= usage.periodEnd {
		return includedUsage{}, errors.New("cursor quota period is not current at completion")
	}
	return usage, nil
}

func quotaPost(ctx context.Context, client *http.Client, target url.URL, path, bearer string, dashboard bool) ([]byte, error) {
	target.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader("{}"))
	if err != nil {
		return nil, errors.New("invalid request")
	}
	// A replayable body lets net/http retry a failed write on a reused connection.
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if dashboard {
		req.Header.Set("Connect-Protocol-Version", "1")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("request failed")
	}
	defer resp.Body.Close()
	// Unary Connect errors use non-200 statuses and JSON error bodies, never a result.
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("source refused")
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, errors.New("unexpected response encoding")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, quotaResponseMaxBytes+1))
	if err != nil || len(body) > quotaResponseMaxBytes || ctx.Err() != nil {
		return nil, errors.New("invalid response body")
	}
	return body, nil
}

// quotaObject retains recognized fields before defaults erase absence. Both spellings name
// one semantic field, so repeated keys and camelCase/snake_case collisions both refuse.
func quotaObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	aliases := make(map[string]string, len(names)*2)
	for _, name := range names {
		aliases[name] = name
		var snake strings.Builder
		for _, c := range name {
			if c >= 'A' && c <= 'Z' {
				snake.WriteByte('_')
				c += 'a' - 'A'
			}
			snake.WriteRune(c)
		}
		aliases[snake.String()] = name
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected object")
	}
	fields := make(map[string]json.RawMessage, len(names))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, errors.New("invalid object")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, errors.New("invalid field")
		}
		if name, recognized := aliases[token.(string)]; recognized {
			if _, duplicate := fields[name]; duplicate {
				return nil, errors.New("duplicate field")
			}
			fields[name] = value
		}
	}
	if _, err := d.Token(); err != nil {
		return nil, errors.New("invalid object end")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return fields, nil
}

func quotaInteger(raw []byte, bits int) (int64, error) {
	text := string(raw)
	if len(text) > 0 && text[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, errors.New("invalid integer")
		}
	}
	value, err := strconv.ParseInt(text, 10, bits)
	if err != nil || strconv.FormatInt(value, 10) != text {
		return 0, errors.New("invalid integer")
	}
	return value, nil
}

func validateQuotaIdentity(raw []byte) error {
	fields, err := quotaObject(raw, "userId", "teamId")
	if err != nil {
		return err
	}
	user, err := quotaInteger(fields["userId"], 32)
	if err != nil || user <= 0 {
		return errors.New("invalid user")
	}
	if team, present := fields["teamId"]; present {
		value, err := quotaInteger(team, 32)
		if err != nil || value <= 0 {
			return errors.New("invalid team")
		}
	}
	return nil
}

func parseCursorUsage(raw []byte) (includedUsage, error) {
	fields, err := quotaObject(raw, "planUsage", "billingCycleStart", "billingCycleEnd", "autoBucketModels")
	if err != nil {
		return includedUsage{}, err
	}
	plan, err := quotaObject(fields["planUsage"], "autoPercentUsed", "apiPercentUsed")
	if err != nil {
		return includedUsage{}, err
	}
	usage := includedUsage{}
	for name, dest := range map[string]*float64{"autoPercentUsed": &usage.autoUsed, "apiPercentUsed": &usage.apiUsed} {
		value := plan[name]
		if len(value) == 0 || value[0] == '"' || bytes.Equal(value, []byte("null")) || json.Unmarshal(value, dest) != nil ||
			math.IsNaN(*dest) || math.IsInf(*dest, 0) || *dest < 0 || *dest > 100 {
			return includedUsage{}, errors.New("invalid percentage")
		}
	}
	usage.periodStart, err = quotaInteger(fields["billingCycleStart"], 64)
	if err != nil {
		return includedUsage{}, err
	}
	usage.periodEnd, err = quotaInteger(fields["billingCycleEnd"], 64)
	if err != nil || usage.periodStart >= usage.periodEnd || usage.periodStart < -62135596800000 || usage.periodEnd > 253402300799999 {
		return includedUsage{}, errors.New("invalid period")
	}
	if models, present := fields["autoBucketModels"]; present {
		var items []json.RawMessage
		if len(models) == 0 || models[0] != '[' || json.Unmarshal(models, &items) != nil {
			return includedUsage{}, errors.New("invalid model list")
		}
		for _, item := range items {
			var model string
			if len(item) == 0 || item[0] != '"' || json.Unmarshal(item, &model) != nil {
				return includedUsage{}, errors.New("invalid model name")
			}
		}
	}
	return usage, nil
}

func validateQuotaHardLimit(raw []byte) error {
	intFields := []string{"hardLimit", "hardLimitPerUser", "perUserMonthlyLimitDollars", "setupPromoAmountCents",
		"setupPromoCreditValidityDays", "perUserFirstPartyModelsAdditionalBudgetDollars"}
	boolFields := []string{"noUsageBasedAllowed", "isDynamicTeamLimit", "hasPendingSetupOnboardingCreditClaim",
		"isSetupPromoActive", "onDemandSpendDisabledByOrganization", "perUserFirstPartyModelsAdditionalBudgetUnlimited"}
	names := append(append([]string{}, intFields...), boolFields...)
	fields, err := quotaObject(raw, names...)
	if err != nil {
		return err
	}
	for _, name := range intFields {
		if value, present := fields[name]; present {
			if _, err := quotaInteger(value, 32); err != nil {
				return err
			}
		}
	}
	for _, name := range boolFields {
		if value, present := fields[name]; present && !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
			return errors.New("invalid policy boolean")
		}
	}
	return nil
}
