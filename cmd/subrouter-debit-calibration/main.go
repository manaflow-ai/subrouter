// subrouter-debit-calibration summarizes existing token accounting offline.
// It deliberately does not infer subscription quota costs from token totals.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"
)

// Keep the input limited to accounting fields. Client labels, account names,
// credentials and request content are never included in the report.
type ledgerRow struct {
	Hour                  string `json:"hour"`
	Provider              string `json:"provider"`
	AccountID             string `json:"account_id"`
	Model                 string `json:"model"`
	Requests              int64  `json:"requests"`
	RequestsWithoutUsage  int64  `json:"requests_without_usage"`
	InputTokens           int64  `json:"input_tokens"`
	CachedInputTokens     int64  `json:"cached_input_tokens"`
	CacheWriteInputTokens int64  `json:"cache_write_input_tokens"`
	OutputTokens          int64  `json:"output_tokens"`
}

type bucketKey struct{ provider, model, account, hour string }
type groupKey struct{ provider, model string }
type totals struct{ requests, missing, input, cached, writes, output int64 }

type modelReport struct {
	Provider                  string   `json:"provider"`
	Model                     string   `json:"model"`
	AccountHours              int      `json:"account_hours"`
	Requests                  int64    `json:"requests"`
	RequestsWithUsage         int64    `json:"requests_with_usage"`
	MissingUsageFraction      float64  `json:"missing_usage_fraction"`
	MeanInputTokens           *float64 `json:"mean_input_tokens_per_observed_request"`
	MeanCachedInputTokens     *float64 `json:"mean_cached_input_tokens_per_observed_request"`
	MeanCacheWriteInputTokens *float64 `json:"mean_cache_write_input_tokens_per_observed_request"`
	MeanOutputTokens          *float64 `json:"mean_output_tokens_per_observed_request"`
	MinBucketMeanTokens       *float64 `json:"min_account_hour_mean_total_tokens"`
	MaxBucketMeanTokens       *float64 `json:"max_account_hour_mean_total_tokens"`
}

type report struct {
	QuotaCalibrationSupported bool          `json:"quota_calibration_supported"`
	Limitations               []string      `json:"limitations"`
	Models                    []modelReport `json:"models"`
}

func main() {
	path := flag.String("ledger", "-", "token-usage JSONL or endpoint JSON file; - reads stdin")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	var input io.Reader = os.Stdin
	if *path != "-" {
		file, err := os.Open(*path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open ledger:", err)
			os.Exit(1)
		}
		defer file.Close()
		input = file
	}
	result, err := analyze(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		os.Exit(1)
	}
}

func analyze(input io.Reader) (report, error) {
	buckets := map[bucketKey]totals{}
	decoder := json.NewDecoder(bufio.NewReader(input))
	for record := 1; ; record++ {
		// Both persisted JSONL rows and {"rows":[...]} endpoint exports work.
		var object struct {
			ledgerRow
			Rows *[]ledgerRow `json:"rows"`
		}
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			return report{}, fmt.Errorf("decode ledger record %d: invalid JSON", record)
		}
		rows := []ledgerRow{object.ledgerRow}
		if object.Rows != nil {
			rows = *object.Rows
		}
		for _, row := range rows {
			if err := validate(row); err != nil {
				return report{}, fmt.Errorf("ledger record %d: %w", record, err)
			}
			hour, _ := time.Parse(time.RFC3339, row.Hour) // Validated above.
			key := bucketKey{row.Provider, row.Model, row.AccountID, hour.UTC().Format(time.RFC3339)}
			value := buckets[key]
			if err := value.add(totals{row.Requests, row.RequestsWithoutUsage, row.InputTokens, row.CachedInputTokens, row.CacheWriteInputTokens, row.OutputTokens}); err != nil {
				return report{}, err
			}
			buckets[key] = value
		}
	}
	groups := map[groupKey]totals{}
	stats := map[groupKey]modelReport{}
	for key, value := range buckets {
		group := groupKey{key.provider, key.model}
		total := groups[group]
		if err := total.add(value); err != nil {
			return report{}, err
		}
		groups[group] = total
		stat := stats[group]
		stat.AccountHours++
		if observed := value.requests - value.missing; observed > 0 {
			mean := (float64(value.input) + float64(value.output)) / float64(observed)
			if stat.MinBucketMeanTokens == nil || mean < *stat.MinBucketMeanTokens {
				v := mean
				stat.MinBucketMeanTokens = &v
			}
			if stat.MaxBucketMeanTokens == nil || mean > *stat.MaxBucketMeanTokens {
				v := mean
				stat.MaxBucketMeanTokens = &v
			}
		}
		stats[group] = stat
	}
	result := report{Models: []modelReport{}, Limitations: []string{
		"Hourly accounting cannot reconstruct request order, concurrent load, or per-request token variance.",
		"Tokens are reported after responses; cached reads and writes are subsets of input and have no assumed quota weight.",
		"Quota calibration requires historical fresh provider-window readings with reset identity, plan/model context, and attribution of usage outside this ledger.",
		"Account-hour means are descriptive, not subscription quota costs or a routing recommendation. Use one export per time range; overlapping exports double count.",
	}}
	for key, value := range groups {
		stat := stats[key]
		stat.Provider = key.provider
		stat.Model = key.model
		stat.Requests = value.requests
		stat.RequestsWithUsage = value.requests - value.missing
		if value.requests > 0 {
			stat.MissingUsageFraction = float64(value.missing) / float64(value.requests)
		}
		stat.MeanInputTokens = mean(value.input, stat.RequestsWithUsage)
		stat.MeanCachedInputTokens = mean(value.cached, stat.RequestsWithUsage)
		stat.MeanCacheWriteInputTokens = mean(value.writes, stat.RequestsWithUsage)
		stat.MeanOutputTokens = mean(value.output, stat.RequestsWithUsage)
		result.Models = append(result.Models, stat)
	}
	sort.Slice(result.Models, func(i, j int) bool {
		a, b := result.Models[i], result.Models[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	return result, nil
}

func (t *totals) add(other totals) error {
	for _, pair := range []struct {
		dst *int64
		src int64
	}{
		{&t.requests, other.requests}, {&t.missing, other.missing}, {&t.input, other.input},
		{&t.cached, other.cached}, {&t.writes, other.writes}, {&t.output, other.output},
	} {
		if pair.src > math.MaxInt64-*pair.dst {
			return fmt.Errorf("accounting counter overflow")
		}
		*pair.dst += pair.src
	}
	return nil
}

func mean(total, observed int64) *float64 {
	if observed <= 0 {
		return nil
	}
	value := float64(total) / float64(observed)
	return &value
}

func validate(row ledgerRow) error {
	hour, err := time.Parse(time.RFC3339, row.Hour)
	if err != nil || !hour.Equal(hour.Truncate(time.Hour)) {
		return fmt.Errorf("hour must be an RFC3339 hour boundary")
	}
	if row.Provider == "" || row.Model == "" || row.AccountID == "" {
		return fmt.Errorf("provider, model and account_id are required")
	}
	if row.Requests < 0 || row.RequestsWithoutUsage < 0 || row.RequestsWithoutUsage > row.Requests || row.InputTokens < 0 || row.OutputTokens < 0 || row.CachedInputTokens < 0 || row.CacheWriteInputTokens < 0 || row.CachedInputTokens > row.InputTokens || row.CacheWriteInputTokens > row.InputTokens {
		return fmt.Errorf("invalid accounting counters")
	}
	if row.Requests == row.RequestsWithoutUsage && (row.InputTokens != 0 || row.OutputTokens != 0) {
		return fmt.Errorf("tokens without any observed requests")
	}
	return nil
}
