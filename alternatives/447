package cli

import (
	"testing"
)

func TestDoctorWorkflowTokenThresholdFlag(t *testing.T) {
	t.Parallel()

	configPath := ""
	env := ""
	logLevel := ""
	host := ""
	port := -1
	cmd := newDoctorCommand(&configPath, &env, &logLevel, &host, &port, options{})
	flag := cmd.Flags().Lookup("workflow-token-threshold")
	if flag == nil {
		t.Fatal("workflow-token-threshold flag is missing")
		return
	}
	if flag.DefValue != "4000" {
		t.Fatalf("workflow-token-threshold default = %q, want 4000", flag.DefValue)
	}
}
