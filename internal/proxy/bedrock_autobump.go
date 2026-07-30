package proxy

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
)

// serviceQuotasAPI is the subset of the Service Quotas client used for bumping.
type serviceQuotasAPI interface {
	GetServiceQuota(ctx context.Context, in *servicequotas.GetServiceQuotaInput, optFns ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error)
	RequestServiceQuotaIncrease(ctx context.Context, in *servicequotas.RequestServiceQuotaIncreaseInput, optFns ...func(*servicequotas.Options)) (*servicequotas.RequestServiceQuotaIncreaseOutput, error)
}

// bedrockTPMQuotaByModel maps a model substring to its adjustable per-minute
// token quota code for the us. cross-region inference profiles. These Service
// Quotas codes are the same in every region.
var bedrockTPMQuotaByModel = map[string]string{
	"fable": "L-9B258944", // Cross-region model inference tokens per minute for Claude Fable 5
	"opus":  "L-DB99DCDB", // Cross-region model inference tokens per minute for Claude Opus 4.8
}

// bedrockQuotaBumper requests Service Quotas increases when Bedrock throttles a
// model. It doubles the current quota (capped) and dedupes per quota with a
// cooldown so sustained throttling never spams AWS with requests.
type bedrockQuotaBumper struct {
	client   serviceQuotasAPI
	cfg      aws.Config
	clients  map[string]serviceQuotasAPI
	logger   *slog.Logger
	cooldown time.Duration
	maxValue float64

	mu   sync.Mutex
	last map[string]time.Time
}

// NewBedrockQuotaBumper builds a quota bumper that reacts to Bedrock throttling.
func NewBedrockQuotaBumper(cfg aws.Config, logger *slog.Logger) *bedrockQuotaBumper {
	return &bedrockQuotaBumper{
		cfg:      cfg,
		clients:  map[string]serviceQuotasAPI{},
		logger:   logger,
		cooldown: 6 * time.Hour,
		maxValue: 20_000_000, // don't auto-request beyond 20M TPM
		last:     map[string]time.Time{},
	}
}

func bedrockQuotaCodeForModel(model string) (string, bool) {
	m := strings.ToLower(model)
	for key, code := range bedrockTPMQuotaByModel {
		if strings.Contains(m, key) {
			return code, true
		}
	}
	return "", false
}

// onThrottle is invoked (in a goroutine) when Bedrock returns a throttling
// response. It requests a quota increase for the model's TPM quota, subject to
// the per-region, per-quota cooldown.
func (b *bedrockQuotaBumper) onThrottle(region, model string) {
	if b == nil {
		return
	}
	region = strings.TrimSpace(region)
	if region == "" {
		return
	}
	code, ok := bedrockQuotaCodeForModel(model)
	if !ok {
		return
	}
	cooldownKey := region + "\x00" + code
	b.mu.Lock()
	if b.last == nil {
		b.last = map[string]time.Time{}
	}
	if last, seen := b.last[cooldownKey]; seen && time.Since(last) < b.cooldown {
		b.mu.Unlock()
		return
	}
	b.last[cooldownKey] = time.Now()
	b.mu.Unlock()

	client := b.clientForRegion(region)
	if client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cur, err := client.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{
		ServiceCode: aws.String("bedrock"),
		QuotaCode:   aws.String(code),
	})
	if err != nil || cur.Quota == nil || cur.Quota.Value == nil {
		if b.logger != nil {
			b.logger.Warn("bedrock autobump: get quota failed", "model", model, "region", region, "quota", code, "error", err)
		}
		return
	}
	current := *cur.Quota.Value
	desired := current * 2
	if desired > b.maxValue {
		desired = b.maxValue
	}
	if desired <= current {
		if b.logger != nil {
			b.logger.Warn("bedrock autobump: already at cap, not requesting", "model", model, "region", region, "quota", code, "current", current, "cap", b.maxValue)
		}
		return
	}
	out, err := client.RequestServiceQuotaIncrease(ctx, &servicequotas.RequestServiceQuotaIncreaseInput{
		ServiceCode:  aws.String("bedrock"),
		QuotaCode:    aws.String(code),
		DesiredValue: aws.Float64(desired),
	})
	if err != nil {
		if b.logger != nil {
			b.logger.Warn("bedrock autobump: request increase failed", "model", model, "region", region, "quota", code, "current", current, "desired", desired, "error", err)
		}
		return
	}
	if b.logger != nil {
		id := ""
		if out.RequestedQuota != nil && out.RequestedQuota.Id != nil {
			id = *out.RequestedQuota.Id
		}
		b.logger.Warn("bedrock autobump: requested quota increase after throttle",
			"model", model, "region", region, "quota", code, "current", current, "desired", desired, "request_id", id)
	}
}

func (b *bedrockQuotaBumper) clientForRegion(region string) serviceQuotasAPI {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		return b.client
	}
	if b.clients == nil {
		b.clients = map[string]serviceQuotasAPI{}
	}
	if client := b.clients[region]; client != nil {
		return client
	}
	client := servicequotas.NewFromConfig(b.cfg, func(o *servicequotas.Options) {
		o.Region = region
	})
	b.clients[region] = client
	return client
}
