package execrunner

import "testing"

// TC-15 (REQ-7), TC-16 (REQ-7): reuse only after auditing the actual process,
// argv, environment, /proc limits, output-size and descendant-PID assertions.
func TestApproved_ProcessBoundary(t *testing.T) {
	t.Run("binary", TestRunner_RejectsNonGitBinary)
	t.Run("commands", TestRunner_SubcommandAllowlist)
	t.Run("literal_arguments", TestRunner_ArgsVerbatim)
	t.Run("environment", TestRunner_EnvMinimal)
	t.Run("limits", TestRunner_LimitsApplied)
	t.Run("output_bound", TestRunner_OutputCap)
	t.Run("descendants_die", TestRunner_TimeoutKillsGroup)
	t.Run("concurrency", TestRunner_Concurrency)
}
