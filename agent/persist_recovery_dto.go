package agent

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

// recoveryProjection includes the last transition's step and failure note so
// checkpoint replay cannot restore a spent request cycle as a successful one.
type recoveryProjection struct {
	RunID                 string `json:"run_id"`
	Round                 int    `json:"round"`
	EpisodeID             string `json:"episode_id"`
	Step                  int    `json:"step"`
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	ConfigSeq             int64  `json:"config_seq"`
	Phase                 string `json:"phase"`
	Attempt               int    `json:"attempt"`
	BudgetRunes           int    `json:"budget_runes"`
	BeforeRunes           int    `json:"before_runes"`
	RejectedOutboundRunes int    `json:"rejected_outbound_runes"`
	Code                  string `json:"code"`
	Reason                string `json:"reason"`
}

type contextRecoveryPayload struct {
	RunID                 string  `json:"run_id"`
	Round                 int     `json:"round"`
	EpisodeID             string  `json:"episode_id"`
	Step                  int     `json:"step"`
	Provider              string  `json:"provider"`
	Model                 string  `json:"model"`
	ConfigSeq             int64   `json:"config_seq"`
	Phase                 string  `json:"phase"`
	Attempt               int     `json:"attempt,omitempty"`
	BudgetRunes           int     `json:"budget_runes,omitempty"`
	BeforeRunes           int     `json:"before_runes"`
	RejectedOutboundRunes int     `json:"rejected_outbound_runes"`
	Code                  string  `json:"code"`
	Reason                string  `json:"reason"`
	SelectedProvider      *string `json:"selected_provider,omitempty"`
	SelectedModel         *string `json:"selected_model,omitempty"`
	SelectedConfigSeq     *int64  `json:"selected_config_seq,omitempty"`
	ModelChangeSeq        *int64  `json:"model_change_seq,omitempty"`
}

// Pointer wire fields distinguish missing/null from legitimate zero and empty
// model keys. Attempt and budget are absent only until the first slot is spent.
func decodeContextRecovery(r store.Record) (contextRecoveryPayload, error) {
	var p contextRecoveryPayload
	wire := struct {
		*contextRecoveryPayload
		Round                 *int    `json:"round"`
		Step                  *int    `json:"step"`
		Provider              *string `json:"provider"`
		Model                 *string `json:"model"`
		ConfigSeq             *int64  `json:"config_seq"`
		Attempt               *int    `json:"attempt"`
		BudgetRunes           *int    `json:"budget_runes"`
		BeforeRunes           *int    `json:"before_runes"`
		RejectedOutboundRunes *int    `json:"rejected_outbound_runes"`
		Code                  *string `json:"code"`
		Reason                *string `json:"reason"`
	}{contextRecoveryPayload: &p}
	if err := decodeRecoveryFields(r.Payload, &wire); err != nil {
		return p, incompatible(r, "invalid context_recovery payload")
	}
	if r.Schema != store.SchemaV4 || p.RunID == "" || p.EpisodeID == "" ||
		wire.Round == nil || wire.Step == nil || wire.Provider == nil || wire.Model == nil ||
		wire.ConfigSeq == nil || wire.BeforeRunes == nil || wire.RejectedOutboundRunes == nil ||
		wire.Code == nil || wire.Reason == nil {
		return p, incompatible(r, "context_recovery requires schema 4 and request identity/baselines")
	}
	p.Round, p.Step, p.Provider, p.Model = *wire.Round, *wire.Step, *wire.Provider, *wire.Model
	p.ConfigSeq, p.BeforeRunes, p.RejectedOutboundRunes =
		*wire.ConfigSeq, *wire.BeforeRunes, *wire.RejectedOutboundRunes
	p.Code, p.Reason = *wire.Code, *wire.Reason
	if p.Round < 0 || p.Step < 0 || p.ConfigSeq < -1 || p.BeforeRunes <= 0 || p.RejectedOutboundRunes <= 0 {
		return p, incompatible(r, "invalid recovery identity or baseline")
	}
	if (wire.Attempt == nil) != (wire.BudgetRunes == nil) {
		return p, incompatible(r, "attempt and budget_runes must be paired")
	}
	if wire.Attempt != nil {
		p.Attempt, p.BudgetRunes = *wire.Attempt, *wire.BudgetRunes
		if p.Attempt < 1 || p.Attempt > 2 || p.BudgetRunes <= 0 {
			return p, incompatible(r, "invalid recovery attempt or budget")
		}
	}
	switch p.Phase {
	case "pending", "blocked", "superseded":
	case "preparing", "dispatched":
		if wire.Attempt == nil {
			return p, incompatible(r, "recovery phase requires an occupied attempt")
		}
	default:
		return p, incompatible(r, "unknown context_recovery phase")
	}
	if p.Reason == "provider_error" && p.Phase != "dispatched" {
		return p, incompatible(r, "provider_error is only a dispatched outcome note")
	}
	if p.Phase == "superseded" {
		if p.SelectedProvider == nil || p.SelectedModel == nil || p.SelectedConfigSeq == nil || p.ModelChangeSeq == nil {
			return p, incompatible(r, "superseded requires the frozen selection and change source")
		}
		if *p.SelectedConfigSeq <= p.ConfigSeq || *p.ModelChangeSeq <= p.ConfigSeq ||
			*p.ModelChangeSeq > *p.SelectedConfigSeq {
			return p, incompatible(r, "invalid superseded version interval")
		}
	} else if p.SelectedProvider != nil || p.SelectedModel != nil || p.SelectedConfigSeq != nil || p.ModelChangeSeq != nil {
		return p, incompatible(r, "selection proof outside superseded")
	}
	return p, nil
}

type historyReplacedPayload struct {
	RunID       string        `json:"run_id"`
	Round       int           `json:"round"`
	OperationID string        `json:"operation_id"`
	Reason      string        `json:"reason"`
	History     []llm.Message `json:"history"`
	EpisodeID   string        `json:"episode_id,omitempty"`
	Attempt     int           `json:"attempt,omitempty"`
	BudgetRunes int           `json:"budget_runes,omitempty"`
	BeforeRunes int           `json:"before_runes,omitempty"`
	AfterRunes  int           `json:"after_runes,omitempty"`
}

func decodeHistoryReplaced(r store.Record) (historyReplacedPayload, error) {
	var p historyReplacedPayload
	wire := struct {
		*historyReplacedPayload
		Round       *int    `json:"round"`
		EpisodeID   *string `json:"episode_id"`
		Attempt     *int    `json:"attempt"`
		BudgetRunes *int    `json:"budget_runes"`
		BeforeRunes *int    `json:"before_runes"`
		AfterRunes  *int    `json:"after_runes"`
	}{historyReplacedPayload: &p}
	if err := decodeRecoveryFields(r.Payload, &wire); err != nil {
		return p, incompatible(r, "invalid history_replaced payload")
	}
	if r.Schema != store.SchemaV4 || p.RunID == "" || p.OperationID == "" || wire.Round == nil || len(p.History) == 0 {
		return p, incompatible(r, "history_replaced requires schema 4 and run/round/operation/history")
	}
	p.Round = *wire.Round
	if p.Round < 0 {
		return p, incompatible(r, "negative history replacement round")
	}
	switch p.Reason {
	case "overflow_compaction":
		if wire.EpisodeID == nil || wire.Attempt == nil || wire.BudgetRunes == nil ||
			wire.BeforeRunes == nil || wire.AfterRunes == nil {
			return p, incompatible(r, "overflow replacement requires episode and budget measurements")
		}
		p.EpisodeID, p.Attempt, p.BudgetRunes = *wire.EpisodeID, *wire.Attempt, *wire.BudgetRunes
		p.BeforeRunes, p.AfterRunes = *wire.BeforeRunes, *wire.AfterRunes
		if p.EpisodeID == "" || p.Attempt < 1 || p.Attempt > 2 || p.BudgetRunes <= 0 ||
			p.AfterRunes <= 0 || p.AfterRunes > p.BudgetRunes || p.AfterRunes >= p.BeforeRunes {
			return p, incompatible(r, "invalid overflow replacement progress")
		}
	case "proactive_compaction", "legacy_anchor":
		if wire.EpisodeID != nil || wire.Attempt != nil || wire.BudgetRunes != nil ||
			wire.BeforeRunes != nil || wire.AfterRunes != nil {
			return p, incompatible(r, "episode fields outside overflow replacement")
		}
	default:
		return p, incompatible(r, "unknown history replacement reason")
	}
	return p, nil
}

func decodeRecoveryProjection(raw json.RawMessage) (*recoveryProjection, error) {
	var p recoveryProjection
	wire := struct {
		*recoveryProjection
		Round                 *int    `json:"round"`
		Step                  *int    `json:"step"`
		Provider              *string `json:"provider"`
		Model                 *string `json:"model"`
		ConfigSeq             *int64  `json:"config_seq"`
		Attempt               *int    `json:"attempt"`
		BudgetRunes           *int    `json:"budget_runes"`
		BeforeRunes           *int    `json:"before_runes"`
		RejectedOutboundRunes *int    `json:"rejected_outbound_runes"`
		Code                  *string `json:"code"`
		Reason                *string `json:"reason"`
	}{recoveryProjection: &p}
	if err := decodeRecoveryFields(raw, &wire); err != nil {
		return nil, fmt.Errorf("decode recovery projection: %w", err)
	}
	if wire.Round == nil || wire.Step == nil || wire.Provider == nil || wire.Model == nil ||
		wire.ConfigSeq == nil || wire.Attempt == nil || wire.BudgetRunes == nil ||
		wire.BeforeRunes == nil || wire.RejectedOutboundRunes == nil || wire.Code == nil || wire.Reason == nil {
		return nil, fmt.Errorf("incomplete recovery projection: %w", ErrIncompatibleLog)
	}
	p.Round, p.Step, p.Provider, p.Model = *wire.Round, *wire.Step, *wire.Provider, *wire.Model
	p.ConfigSeq, p.Attempt, p.BudgetRunes = *wire.ConfigSeq, *wire.Attempt, *wire.BudgetRunes
	p.BeforeRunes, p.RejectedOutboundRunes = *wire.BeforeRunes, *wire.RejectedOutboundRunes
	p.Code, p.Reason = *wire.Code, *wire.Reason
	if p.RunID == "" || p.EpisodeID == "" || p.Round < 0 || p.Step < 0 || p.ConfigSeq < -1 ||
		p.Attempt < 0 || p.Attempt > 2 || p.BudgetRunes < 0 || p.BeforeRunes <= 0 || p.RejectedOutboundRunes <= 0 ||
		(p.Attempt == 0) != (p.BudgetRunes == 0) {
		return nil, fmt.Errorf("invalid recovery projection: %w", ErrIncompatibleLog)
	}
	switch p.Phase {
	case "pending":
		if p.Attempt == 0 && p.Step != 0 {
			return nil, fmt.Errorf("invalid first pending projection: %w", ErrIncompatibleLog)
		}
	case "blocked":
		if p.Reason == "" {
			return nil, fmt.Errorf("missing blocked reason: %w", ErrIncompatibleLog)
		}
	case "preparing", "ready", "dispatched":
		if p.Attempt == 0 {
			return nil, fmt.Errorf("unoccupied recovery projection: %w", ErrIncompatibleLog)
		}
	default:
		return nil, fmt.Errorf("invalid recovery projection phase: %w", ErrIncompatibleLog)
	}
	if p.Reason == "provider_error" && p.Phase != "dispatched" {
		return nil, fmt.Errorf("misplaced provider failure note: %w", ErrIncompatibleLog)
	}
	return &p, nil
}

// Optional recovery fields are omitted, never null. In particular null must
// not disguise an explicit attempt/budget or an inapplicable episode field.
func decodeRecoveryFields(raw json.RawMessage, target any) error {
	if err := strictDecode(raw, target); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("null recovery field %q: %w", key, ErrIncompatibleLog)
		}
	}
	return nil
}
