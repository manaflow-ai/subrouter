package main

type goldenCounterChain struct {
	values map[string][2]int64
}

func (chain *goldenCounterChain) observe(key string, restarts, oom goldenDeployCounter) error {
	if key == "" || restarts.Before == nil || restarts.After == nil || oom.Before == nil || oom.After == nil ||
		*restarts.Before < 0 || *oom.Before < 0 || *restarts.Before != *restarts.After || *oom.Before != *oom.After {
		return failGolden("server_counter_continuity_invalid")
	}
	current := [2]int64{*restarts.Before, *oom.Before}
	if previous, ok := chain.values[key]; ok && previous != current {
		return failGolden("server_counter_continuity_invalid")
	}
	chain.values[key] = [2]int64{*restarts.After, *oom.After}
	return nil
}

func (chain *goldenCounterChain) observeDeploy(key string, metrics goldenDeployServiceMetrics) error {
	return chain.observe(key, metrics.NRestarts, metrics.OOMKill)
}

func (chain *goldenCounterChain) observeMigrationTransition(evidence *goldenMigrationEvidence) error {
	if evidence == nil {
		return nil
	}
	if err := chain.observe("legacy", evidence.Metrics.Legacy.NRestarts, evidence.Metrics.Legacy.OOMKill); err != nil {
		return err
	}
	if err := chain.observe("slot:"+evidence.Metrics.Slot.ID, evidence.Metrics.Slot.NRestarts, evidence.Metrics.Slot.OOMKill); err != nil {
		return err
	}
	return chain.observeDeploy("front", evidence.Metrics.Front)
}

func (chain *goldenCounterChain) observeSlotAction(evidence *goldenDeployEvidence) error {
	if evidence == nil {
		return nil
	}
	switch evidence.EvidenceType {
	case "slot-activation":
		if err := chain.observeDeploy("slot:"+evidence.Slots.Before, evidence.Metrics.OldSlot); err != nil {
			return err
		}
		if err := chain.observeDeploy("slot:"+evidence.Slots.Candidate, evidence.Metrics.CandidateSlot); err != nil {
			return err
		}
		return chain.observeDeploy("front", evidence.Metrics.Front)
	case "slot-rollback":
		if err := chain.observeDeploy("slot:"+evidence.Slots.From, evidence.Metrics.RetiringSlot); err != nil {
			return err
		}
		if err := chain.observeDeploy("slot:"+evidence.Slots.To, evidence.Metrics.RestoredSlot); err != nil {
			return err
		}
		return chain.observeDeploy("front", evidence.Metrics.Front)
	case "slot-retirement":
		return chain.observeDeploy("slot:"+evidence.Slots.Retired, evidence.Metrics.OldSlot)
	default:
		return failGolden("server_counter_continuity_invalid")
	}
}

func validateGoldenCounterContinuity(summary goldenSummary) error {
	chain := goldenCounterChain{values: make(map[string][2]int64)}
	for _, action := range []goldenActionSummary{
		summary.MigrationRehearsalCutover,
		summary.MigrationRollback,
		summary.MigrationFinalCutover,
	} {
		if action.migrationCanonical == nil {
			continue
		}
		if err := chain.observeMigrationTransition(action.migrationCanonical); err != nil {
			return err
		}
	}
	if cleanup := summary.LegacyCleanup.migrationCanonical; cleanup != nil {
		if err := chain.observe("legacy", cleanup.Metrics.NRestarts, cleanup.Metrics.OOMKill); err != nil {
			return err
		}
	}
	for _, action := range []goldenActionSummary{
		summary.Activation,
		summary.Rollback,
		summary.OldGenerationCleanup,
		summary.FinalActivation,
		summary.FinalOldGenerationCleanup,
	} {
		if action.canonical == nil {
			continue
		}
		if err := chain.observeSlotAction(action.canonical); err != nil {
			return err
		}
	}
	return nil
}

func validateGoldenSlotCounterHandoff(actions ...goldenActionSummary) error {
	chain := goldenCounterChain{values: make(map[string][2]int64)}
	for _, action := range actions {
		if action.canonical == nil {
			return failGolden("server_counter_continuity_invalid")
		}
		if err := chain.observeSlotAction(action.canonical); err != nil {
			return err
		}
	}
	return nil
}
