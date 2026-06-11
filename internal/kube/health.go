package kube

import "fmt"

type HealthState string

const (
	Healthy       HealthState = "healthy"
	Degraded      HealthState = "degraded"
	Crashlooping  HealthState = "crashlooping"
	HealthUnknown HealthState = "unknown"
)

type HealthSummary struct {
	State  HealthState
	Detail string
}

// SummarizeHealth derives a single health state for a namespace from its
// pods. Pods in phase Succeeded (completed jobs/hooks) are ignored.
func SummarizeHealth(pods []PodInfo) HealthSummary {
	var ready, total int32
	var crashRestarts int32
	crashing := false
	waitingReason := ""
	degraded := false

	for _, p := range pods {
		if p.Phase == "Succeeded" {
			continue
		}
		if p.Phase == "Failed" || p.Phase == "Pending" {
			degraded = true
		}
		for _, ctr := range p.Containers {
			total++
			if ctr.Ready {
				ready++
			}
			switch ctr.WaitingReason {
			case "":
			case "CrashLoopBackOff":
				crashing = true
				if ctr.RestartCount > crashRestarts {
					crashRestarts = ctr.RestartCount
				}
			default:
				degraded = true
				if waitingReason == "" {
					waitingReason = ctr.WaitingReason
				}
			}
		}
	}

	if total == 0 {
		return HealthSummary{State: HealthUnknown, Detail: "no pods"}
	}
	if crashing {
		return HealthSummary{State: Crashlooping, Detail: fmt.Sprintf("%d restarts", crashRestarts)}
	}
	if degraded || ready < total {
		detail := fmt.Sprintf("%d/%d ready", ready, total)
		if waitingReason != "" {
			detail += " — " + waitingReason
		}
		return HealthSummary{State: Degraded, Detail: detail}
	}
	return HealthSummary{State: Healthy, Detail: fmt.Sprintf("%d/%d ready", ready, total)}
}
