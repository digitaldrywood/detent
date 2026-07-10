package connector

import "context"

type ReconcileTarget struct {
	Scope          string
	WorkItemNumber int
	ChangeNumber   int
	Revision       string
	Branch         string
	Event          string
	DeliveryID     string
}

type ReconcileResult struct {
	Issue Issue
	Found bool
}

type TargetedReconciler interface {
	ReconcileIssue(context.Context, ReconcileTarget) (ReconcileResult, error)
}
