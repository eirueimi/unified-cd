package api

// StageSteps returns all concrete ClaimSteps in a ClaimStage.
func StageSteps(stage ClaimStage) []ClaimStep {
	if stage.Step != nil {
		return []ClaimStep{*stage.Step}
	}
	return stage.Parallel
}
