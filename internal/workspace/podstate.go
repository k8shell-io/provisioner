package workspace

import (
	"context"
	"fmt"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/rs/zerolog"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// ---- Types ------------------------------------------------------------------

// PodLifecycleStage is the current stage in a pod's lifecycle.
// Stages progress roughly in order: Scheduling → Pulling → Initializing → Starting → Running.
// A pod can enter Terminating, Stopped, or Failed from any stage.
type PodLifecycleStage string

const (
	StageScheduling   PodLifecycleStage = "Scheduling"
	StagePulling      PodLifecycleStage = "Pulling"
	StageInitializing PodLifecycleStage = "Initializing"
	StageStarting     PodLifecycleStage = "Starting"
	StageRunning      PodLifecycleStage = "Running"
	StageTerminating  PodLifecycleStage = "Terminating"
	StageStopped      PodLifecycleStage = "Stopped"
	StageFailed       PodLifecycleStage = "Failed"
	StageUnknown      PodLifecycleStage = "Unknown"
)

// EventSeverity classifies how actionable a pod event is.
type EventSeverity string

const (
	EventSeverityInfo     EventSeverity = "info"
	EventSeverityWarning  EventSeverity = "warning"
	EventSeverityCritical EventSeverity = "critical"
)

// PodEvent is an enriched Kubernetes event for a pod or one of its dependent PVCs.
type PodEvent struct {
	Time       time.Time
	ObjectKind string
	ObjectName string
	Reason     string
	Message    string
	Severity   EventSeverity
}

// PodStateSnapshot is the derived workspace state at a single point in time.
// Produced by AnalyzePod — a pure function with no side effects.
type PodStateSnapshot struct {
	Created         time.Time
	Stage           PodLifecycleStage
	Status          models.WorkspaceStatusMessage
	Message         string
	Restarts        int32
	LastFailMessage string
	CriticalErr     error
	Events          []PodEvent
}

// defaultCrashLoopThreshold is the per-container restart count at which a BackOff event
// is treated as critical (provisioning should abort).
const defaultCrashLoopThreshold int32 = 2

// stageToStatus maps a lifecycle stage to the workspace status message reported to clients.
func stageToStatus(stage PodLifecycleStage) models.WorkspaceStatusMessage {
	switch stage {
	case StageScheduling, StageInitializing, StageStarting:
		return models.WorkspaceStatusProvisioning
	case StagePulling:
		return models.WorkspaceStatusPulling
	case StageRunning:
		return models.WorkspaceStatusRunning
	case StageTerminating:
		return models.WorkspaceStatusTerminating
	case StageStopped:
		return models.WorkspaceStatusStopped
	case StageFailed:
		return models.WorkspaceStatusFailing
	default:
		return models.WorkspaceStatusUnknown
	}
}

// ---- AnalyzePod (pure) -------------------------------------------------------

// AnalyzePod derives a PodStateSnapshot from a pod and its associated K8s events.
// events may be nil/empty for a point-in-time check without event history; in that
// case StagePulling cannot be detected since it is event-driven only.
// crashLoopThreshold is the per-container restart count at which a BackOff event
// becomes critical.
// This function has no side effects and makes no I/O calls.
func AnalyzePod(pod *corev1.Pod, events []corev1.Event, crashLoopThreshold int32) PodStateSnapshot {
	if pod == nil {
		return PodStateSnapshot{
			Stage:   StageUnknown,
			Status:  models.WorkspaceStatusUnknown,
			Message: "Pod not found",
		}
	}

	// Total restarts (sum) for the WorkspaceStatus.Restarts field.
	restarts := podRestartCount(pod)
	// Max per-container restarts for the crash loop threshold.
	maxRestarts := podMaxContainerRestarts(pod)

	// Classify events; track the image pull state of each container via FieldPath.
	podEvents := make([]PodEvent, 0, len(events))
	var criticalErr error

	type pullState struct {
		reason string
		t      time.Time
	}
	containerPull := map[string]*pullState{} // FieldPath → last Pulling/Pulled event

	for i := range events {
		ev := &events[i]
		pe := classifyEvent(ev, maxRestarts, crashLoopThreshold)
		podEvents = append(podEvents, pe)

		if pe.Severity == EventSeverityCritical && criticalErr == nil {
			criticalErr = fmt.Errorf("%s: %s", pe.Reason, pe.Message)
		}

		if ev.Reason == "Pulling" || ev.Reason == "Pulled" {
			fp := ev.InvolvedObject.FieldPath
			t := ev.LastTimestamp.Time
			if t.IsZero() {
				t = ev.CreationTimestamp.Time
			}
			if s, ok := containerPull[fp]; !ok || t.After(s.t) {
				containerPull[fp] = &pullState{reason: ev.Reason, t: t}
			}
		}
	}

	activePulling := 0
	for _, s := range containerPull {
		if s.reason == "Pulling" {
			activePulling++
		}
	}

	lastFail := podLastFailure(pod)

	// ---- Terminating ----
	if pod.DeletionTimestamp != nil {
		return PodStateSnapshot{
			Created:         pod.CreationTimestamp.Time,
			Stage:           StageTerminating,
			Status:          stageToStatus(StageTerminating),
			Message:         "Workspace is terminating",
			Restarts:        restarts,
			LastFailMessage: lastFail,
			Events:          podEvents,
		}
	}

	// ---- Terminal phases ----
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return PodStateSnapshot{
			Created:         pod.CreationTimestamp.Time,
			Stage:           StageStopped,
			Status:          stageToStatus(StageStopped),
			Message:         "Workspace completed",
			Restarts:        restarts,
			LastFailMessage: lastFail,
			Events:          podEvents,
		}
	case corev1.PodFailed:
		return PodStateSnapshot{
			Created:         pod.CreationTimestamp.Time,
			Stage:           StageFailed,
			Status:          stageToStatus(StageFailed),
			Message:         podFailureMessage(pod),
			Restarts:        restarts,
			LastFailMessage: lastFail,
			CriticalErr:     criticalErr,
			Events:          podEvents,
		}
	}

	// ---- Not yet scheduled ----
	if pod.Spec.NodeName == "" {
		return PodStateSnapshot{
			Created:         pod.CreationTimestamp.Time,
			Stage:           StageScheduling,
			Status:          stageToStatus(StageScheduling),
			Message:         schedulingMessage(pod),
			Restarts:        restarts,
			LastFailMessage: lastFail,
			Events:          podEvents,
		}
	}

	// ---- Image pulling (event-driven only) ----
	if activePulling > 0 {
		return PodStateSnapshot{
			Created:         pod.CreationTimestamp.Time,
			Stage:           StagePulling,
			Status:          stageToStatus(StagePulling),
			Message:         "Downloading container images",
			Restarts:        restarts,
			LastFailMessage: lastFail,
			Events:          podEvents,
		}
	}

	// ---- Critical event surfaced before pod phase reflects it ----
	// e.g. ImagePullBackOff event arrives before pod phase becomes Failed
	if criticalErr != nil {
		return PodStateSnapshot{
			Created:         pod.CreationTimestamp.Time,
			Stage:           StageFailed,
			Status:          stageToStatus(StageFailed),
			Message:         criticalErr.Error(),
			Restarts:        restarts,
			LastFailMessage: lastFail,
			CriticalErr:     criticalErr,
			Events:          podEvents,
		}
	}

	// ---- Init containers ----
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0 {
			continue // completed successfully
		}
		if cs.State.Waiting != nil {
			reason := cs.State.Waiting.Reason
			msg := cs.State.Waiting.Message
			if isHardFailureReason(reason) {
				err := fmt.Errorf("%s", containerStateMessage(cs.Name, reason, msg))
				return PodStateSnapshot{
					Created:         pod.CreationTimestamp.Time,
					Stage:           StageFailed,
					Status:          stageToStatus(StageFailed),
					Message:         err.Error(),
					Restarts:        restarts,
					LastFailMessage: lastFail,
					CriticalErr:     err,
					Events:          podEvents,
				}
			}
			return PodStateSnapshot{
				Created:         pod.CreationTimestamp.Time,
				Stage:           StageInitializing,
				Status:          stageToStatus(StageInitializing),
				Message:         containerStateMessage(cs.Name, reason, msg),
				Restarts:        restarts,
				LastFailMessage: lastFail,
				Events:          podEvents,
			}
		}
		if cs.State.Running != nil {
			return PodStateSnapshot{
				Created:         pod.CreationTimestamp.Time,
				Stage:           StageInitializing,
				Status:          stageToStatus(StageInitializing),
				Message:         fmt.Sprintf("Running init container %s", cs.Name),
				Restarts:        restarts,
				LastFailMessage: lastFail,
				Events:          podEvents,
			}
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			msg := initContainerFailMessage(cs)
			err := fmt.Errorf("%s", msg)
			return PodStateSnapshot{
				Created:         pod.CreationTimestamp.Time,
				Stage:           StageFailed,
				Status:          stageToStatus(StageFailed),
				Message:         msg,
				Restarts:        restarts,
				LastFailMessage: lastFail,
				CriticalErr:     err,
				Events:          podEvents,
			}
		}
		// init container not yet started
		return PodStateSnapshot{
			Created:         pod.CreationTimestamp.Time,
			Stage:           StageInitializing,
			Status:          stageToStatus(StageInitializing),
			Message:         fmt.Sprintf("Waiting for init container %s", cs.Name),
			Restarts:        restarts,
			LastFailMessage: lastFail,
			Events:          podEvents,
		}
	}

	// ---- Main containers ----
	if len(pod.Status.ContainerStatuses) == 0 {
		return PodStateSnapshot{
			Created:         pod.CreationTimestamp.Time,
			Stage:           StageStarting,
			Status:          stageToStatus(StageStarting),
			Message:         "Waiting for containers to start",
			Restarts:        restarts,
			LastFailMessage: lastFail,
			Events:          podEvents,
		}
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			reason := cs.State.Waiting.Reason
			msg := cs.State.Waiting.Message
			if isHardFailureReason(reason) {
				err := fmt.Errorf("%s", containerStateMessage(cs.Name, reason, msg))
				return PodStateSnapshot{
					Created:         pod.CreationTimestamp.Time,
					Stage:           StageFailed,
					Status:          stageToStatus(StageFailed),
					Message:         err.Error(),
					Restarts:        restarts,
					LastFailMessage: lastFail,
					CriticalErr:     err,
					Events:          podEvents,
				}
			}
			return PodStateSnapshot{
				Created:         pod.CreationTimestamp.Time,
				Stage:           StageStarting,
				Status:          stageToStatus(StageStarting),
				Message:         containerStateMessage(cs.Name, reason, msg),
				Restarts:        restarts,
				LastFailMessage: lastFail,
				Events:          podEvents,
			}
		}
	}

	allReady := true
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			allReady = false
			break
		}
	}

	if allReady {
		return PodStateSnapshot{
			Created:         pod.CreationTimestamp.Time,
			Stage:           StageRunning,
			Status:          stageToStatus(StageRunning),
			Message:         "Workspace is ready",
			Restarts:        restarts,
			LastFailMessage: lastFail,
			Events:          podEvents,
		}
	}

	return PodStateSnapshot{
		Created:         pod.CreationTimestamp.Time,
		Stage:           StageStarting,
		Status:          stageToStatus(StageStarting),
		Message:         startingMessage(pod),
		Restarts:        restarts,
		LastFailMessage: lastFail,
		Events:          podEvents,
	}
}

// classifyEvent assigns a severity to a Kubernetes event based on its Reason and context.
func classifyEvent(ev *corev1.Event, maxRestarts int32, crashLoopThreshold int32) PodEvent {
	t := ev.LastTimestamp.Time
	if t.IsZero() {
		t = ev.CreationTimestamp.Time
	}

	pe := PodEvent{
		Time:       t,
		ObjectKind: ev.InvolvedObject.Kind,
		ObjectName: ev.InvolvedObject.Name,
		Reason:     ev.Reason,
		Message:    ev.Message,
		Severity:   EventSeverityInfo,
	}

	switch ev.InvolvedObject.Kind {
	case "Pod":
		switch ev.Reason {
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
			pe.Severity = EventSeverityCritical
		case "OOMKilled":
			pe.Severity = EventSeverityCritical
		case "BackOff":
			if maxRestarts >= crashLoopThreshold {
				pe.Severity = EventSeverityCritical
			} else {
				pe.Severity = EventSeverityWarning
			}
		case "FailedScheduling", "Unhealthy", "NodeNotReady":
			pe.Severity = EventSeverityWarning
		}
	case "PersistentVolumeClaim":
		switch ev.Reason {
		case "FailedBinding", "ProvisioningFailed":
			pe.Severity = EventSeverityCritical
		}
	}

	return pe
}

// isHardFailureReason returns true for container waiting reasons that indicate a permanent
// error that will not resolve without user intervention.
func isHardFailureReason(reason string) bool {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
		"CreateContainerConfigError", "CreateContainerError",
		"RunContainerError", "OOMKilled":
		return true
	}
	return false
}

// podMaxContainerRestarts returns the highest restart count of any single container
// (init or regular), used for crash loop threshold checks.
func podMaxContainerRestarts(pod *corev1.Pod) int32 {
	var max int32
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount > max {
			max = cs.RestartCount
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.RestartCount > max {
			max = cs.RestartCount
		}
	}
	return max
}

func schedulingMessage(pod *corev1.Pod) string {
	if pod.Status.Reason != "" {
		if pod.Status.Message != "" {
			return fmt.Sprintf("%s: %s", pod.Status.Reason, pod.Status.Message)
		}
		return pod.Status.Reason
	}
	return "Waiting for node assignment"
}

func podFailureMessage(pod *corev1.Pod) string {
	if pod.Status.Reason != "" {
		if pod.Status.Message != "" {
			return fmt.Sprintf("%s: %s", pod.Status.Reason, pod.Status.Message)
		}
		return pod.Status.Reason
	}
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	return "Pod failed"
}

func containerStateMessage(containerName, reason, message string) string {
	if reason != "" && message != "" {
		return fmt.Sprintf("%s (%s): %s", containerName, reason, message)
	}
	if reason != "" {
		return fmt.Sprintf("%s: %s", containerName, reason)
	}
	if message != "" {
		return fmt.Sprintf("%s: %s", containerName, message)
	}
	return fmt.Sprintf("Container %s waiting", containerName)
}

func initContainerFailMessage(cs corev1.ContainerStatus) string {
	term := cs.State.Terminated
	if term == nil {
		return fmt.Sprintf("Init container %s failed", cs.Name)
	}
	if term.Reason != "" && term.Message != "" {
		return fmt.Sprintf("Init container %s failed: %s: %s", cs.Name, term.Reason, term.Message)
	}
	if term.Reason != "" {
		return fmt.Sprintf("Init container %s failed: %s", cs.Name, term.Reason)
	}
	if term.Message != "" {
		return fmt.Sprintf("Init container %s failed: %s", cs.Name, term.Message)
	}
	return fmt.Sprintf("Init container %s exited with code %d", cs.Name, term.ExitCode)
}

func startingMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return fmt.Sprintf("Container %s is starting", cs.Name)
		}
	}
	return "Containers are starting"
}

// podPVCNames returns the set of PVC claim names directly referenced by the pod's volumes.
func podPVCNames(pod *corev1.Pod) map[string]bool {
	names := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "" {
			names[v.PersistentVolumeClaim.ClaimName] = true
		}
	}
	return names
}

// snapToWorkspaceStatus converts a PodStateSnapshot to the models.WorkspaceStatus used by the API.
func snapToWorkspaceStatus(snap *PodStateSnapshot) *models.WorkspaceStatus {
	if snap == nil {
		return nil
	}
	return &models.WorkspaceStatus{
		Created:         snap.Created,
		Status:          snap.Status,
		Message:         snap.Message,
		Restarts:        snap.Restarts,
		LastFailMessage: snap.LastFailMessage,
	}
}

// ---- PodWatcher -------------------------------------------------------------

// PodWatcher observes a pod's lifecycle using the Kubernetes watch API.
// It calls AnalyzePod on each state change and emits WorkspaceStreamEvents.
type PodWatcher struct {
	kubeClient         kubernetes.Interface
	namespace          string
	podName            string
	log                *zerolog.Logger
	CrashLoopThreshold int32 // per-container restart count that makes BackOff critical; default 2
}

// NewPodWatcher creates a PodWatcher with default configuration.
func NewPodWatcher(kubeClient kubernetes.Interface, namespace, podName string, log *zerolog.Logger) *PodWatcher {
	return &PodWatcher{
		kubeClient:         kubeClient,
		namespace:          namespace,
		podName:            podName,
		log:                log,
		CrashLoopThreshold: defaultCrashLoopThreshold,
	}
}

// Watch observes the pod lifecycle.
//
// If waitForRunning is false, it fetches the current pod and recent events once,
// derives a snapshot, and returns immediately — suitable for point-in-time status checks.
//
// If waitForRunning is true, it uses the Kubernetes watch API to observe changes
// in real time until the pod reaches Running, a critical error is detected,
// the timeout in opts.Timeout elapses, or ctx is cancelled.
// Status changes and events are emitted to opts.Messages if set.
func (pw *PodWatcher) Watch(ctx context.Context, opts *ProvisionOptions, waitForRunning bool) (*PodStateSnapshot, error) {
	pw.log.Debug().
		Str("pod", pw.podName).
		Str("namespace", pw.namespace).
		Bool("waitForRunning", waitForRunning).
		Msg("PodWatcher starting")

	v1 := pw.kubeClient.CoreV1()

	pod, err := v1.Pods(pw.namespace).Get(ctx, pw.podName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", models.ErrWorkspaceNotFound, pw.podName)
		}
		return nil, fmt.Errorf("failed to get pod %s: %w", pw.podName, err)
	}

	pw.log.Debug().
		Str("phase", string(pod.Status.Phase)).
		Str("node", pod.Spec.NodeName).
		Str("rv", pod.ResourceVersion).
		Msg("fetched pod")

	pvcNames := podPVCNames(pod)
	podEventList, pvcEventList := pw.listEventSources(ctx, pod.Name, pvcNames)

	allEvents := make([]corev1.Event, 0, len(podEventList.Items)+len(pvcEventList.Items))
	allEvents = append(allEvents, podEventList.Items...)
	for _, ev := range pvcEventList.Items {
		if pvcNames[ev.InvolvedObject.Name] {
			allEvents = append(allEvents, ev)
		}
	}

	pw.log.Debug().
		Int("podEvents", len(podEventList.Items)).
		Int("pvcEvents", len(pvcEventList.Items)).
		Int("pvcs", len(pvcNames)).
		Msg("fetched events")

	snap := AnalyzePod(pod, allEvents, pw.CrashLoopThreshold)
	if snap.Stage == StageRunning && !snap.Created.IsZero() {
		snap.Message = fmt.Sprintf("Workspace is ready, provisioned in %s",
			time.Since(snap.Created).Round(time.Second))
	}

	pw.log.Debug().
		Str("stage", string(snap.Stage)).
		Str("status", string(snap.Status)).
		Str("message", snap.Message).
		Int32("restarts", snap.Restarts).
		AnErr("criticalErr", snap.CriticalErr).
		Msg("initial pod snapshot")

	if !waitForRunning {
		return &snap, snap.CriticalErr
	}

	pw.emitSnapshot(opts, snap)

	if snap.CriticalErr != nil {
		pw.log.Info().Err(snap.CriticalErr).Str("stage", string(snap.Stage)).Msg("PodWatcher returning: critical error in initial snapshot")
		return &snap, snap.CriticalErr
	}
	switch snap.Stage {
	case StageRunning:
		pw.log.Info().Int32("restarts", snap.Restarts).Msg("PodWatcher returning: already running")
		return &snap, nil
	case StageStopped, StageFailed:
		pw.log.Info().Str("stage", string(snap.Stage)).Str("message", snap.Message).Msg("PodWatcher returning: terminal stage in initial snapshot")
		return &snap, fmt.Errorf("workspace %s failed to start: %s", pw.podName, snap.Message)
	}

	return pw.watchLoop(ctx, opts, pod, pvcNames, allEvents, podEventList.ResourceVersion, pvcEventList.ResourceVersion)
}

// watchLoop is the real-time event-driven observation loop entered after the initial snapshot.
func (pw *PodWatcher) watchLoop(
	ctx context.Context,
	opts *ProvisionOptions,
	initialPod *corev1.Pod,
	pvcNames map[string]bool,
	initialEvents []corev1.Event,
	podEventsRV string,
	pvcEventsRV string,
) (*PodStateSnapshot, error) {
	timeoutSec := 0
	if opts != nil {
		timeoutSec = opts.Timeout
	}
	pw.log.Debug().
		Str("pod", pw.podName).
		Int("knownEvents", len(initialEvents)).
		Int("pvcs", len(pvcNames)).
		Int("timeoutSeconds", timeoutSec).
		Msg("entering watchLoop")

	var timeoutCh <-chan time.Time // nil channel, never fires unless opts.Timeout > 0
	if opts != nil && opts.Timeout > 0 {
		timer := time.NewTimer(time.Duration(opts.Timeout) * time.Second)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	v1 := pw.kubeClient.CoreV1()

	// Accumulated state.
	currentPod := initialPod
	knownEvents := make(map[types.UID]corev1.Event, len(initialEvents))
	for _, ev := range initialEvents {
		knownEvents[ev.UID] = ev
	}

	eventSlice := func() []corev1.Event {
		s := make([]corev1.Event, 0, len(knownEvents))
		for _, ev := range knownEvents {
			s = append(s, ev)
		}
		return s
	}

	// Single channel fed by all three watcher goroutines.
	type watchMsg struct {
		pod   *corev1.Pod
		event *corev1.Event
	}
	updateCh := make(chan watchMsg, 64)

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()

	// Goroutine 1: watch pod object changes.
	go func() {
		watcher, err := v1.Pods(pw.namespace).Watch(watchCtx, metav1.ListOptions{
			FieldSelector:   fmt.Sprintf("metadata.name=%s", pw.podName),
			ResourceVersion: initialPod.ResourceVersion,
		})
		if err != nil {
			pw.log.Warn().Err(err).Msg("Failed to start pod watcher")
			return
		}
		defer watcher.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case evt, ok := <-watcher.ResultChan():
				if !ok {
					return
				}
				if evt.Type == watch.Deleted {
					continue
				}
				pod, ok := evt.Object.(*corev1.Pod)
				if !ok || pod == nil {
					continue
				}
				select {
				case updateCh <- watchMsg{pod: pod}:
				case <-watchCtx.Done():
					return
				}
			}
		}
	}()

	// Goroutine 2: watch pod events.
	go func() {
		watcher, err := v1.Events(pw.namespace).Watch(watchCtx, metav1.ListOptions{
			FieldSelector:   fmt.Sprintf("involvedObject.kind=Pod,involvedObject.name=%s", pw.podName),
			ResourceVersion: podEventsRV,
		})
		if err != nil {
			pw.log.Warn().Err(err).Msg("Failed to start pod events watcher")
			return
		}
		defer watcher.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case evt, ok := <-watcher.ResultChan():
				if !ok {
					return
				}
				if evt.Type != watch.Added && evt.Type != watch.Modified {
					continue
				}
				k8sEv, ok := evt.Object.(*corev1.Event)
				if !ok || k8sEv == nil {
					continue
				}
				select {
				case updateCh <- watchMsg{event: k8sEv}:
				case <-watchCtx.Done():
					return
				}
			}
		}
	}()

	// Goroutine 3: watch PVC events (only when this pod has PVCs).
	if len(pvcNames) > 0 {
		go func() {
			watcher, err := v1.Events(pw.namespace).Watch(watchCtx, metav1.ListOptions{
				FieldSelector:   "involvedObject.kind=PersistentVolumeClaim",
				ResourceVersion: pvcEventsRV,
			})
			if err != nil {
				pw.log.Warn().Err(err).Msg("Failed to start PVC events watcher")
				return
			}
			defer watcher.Stop()
			for {
				select {
				case <-watchCtx.Done():
					return
				case evt, ok := <-watcher.ResultChan():
					if !ok {
						return
					}
					if evt.Type != watch.Added && evt.Type != watch.Modified {
						continue
					}
					k8sEv, ok := evt.Object.(*corev1.Event)
					if !ok || k8sEv == nil {
						continue
					}
					if !pvcNames[k8sEv.InvolvedObject.Name] {
						continue // not a PVC belonging to this pod
					}
					select {
					case updateCh <- watchMsg{event: k8sEv}:
					case <-watchCtx.Done():
						return
					}
				}
			}
		}()
	}

	// Track which event messages have already been emitted (UID + Count) to avoid duplicates.
	type emittedKey struct {
		uid   types.UID
		count int32
	}
	emitted := map[emittedKey]bool{}

	// Pulling status delay: avoid reporting StagePulling until it has persisted for a while,
	// to suppress noise from images that are cached and resolve almost immediately.
	const pullReportDelay = 8 * time.Second
	var pullingStart time.Time
	pullingReported := false

	lastStatus := AnalyzePod(currentPod, eventSlice(), pw.CrashLoopThreshold).Status

	for {
		select {
		case <-ctx.Done():
			pw.log.Debug().Err(ctx.Err()).Str("pod", pw.podName).Msg("PodWatcher watchLoop: context cancelled")
			return nil, ctx.Err()

		case <-timeoutCh:
			snap := AnalyzePod(currentPod, eventSlice(), pw.CrashLoopThreshold)
			pw.log.Warn().
				Str("pod", pw.podName).
				Str("stage", string(snap.Stage)).
				Str("status", string(snap.Status)).
				Int("timeoutSeconds", timeoutSec).
				Msg("PodWatcher watchLoop: timeout waiting for pod")
			if snap.Status == models.WorkspaceStatusPulling {
				return nil, fmt.Errorf("timeout waiting for pod %s: image is still being downloaded", pw.podName)
			}
			return nil, fmt.Errorf("timeout waiting for pod %s to be running", pw.podName)

		case msg, ok := <-updateCh:
			if !ok {
				return nil, fmt.Errorf("update channel closed unexpectedly for pod %s", pw.podName)
			}

			if msg.pod != nil {
				currentPod = msg.pod
				pw.log.Debug().
					Str("pod", msg.pod.Name).
					Str("phase", string(msg.pod.Status.Phase)).
					Str("node", msg.pod.Spec.NodeName).
					Msg("pod object updated")
			}
			if msg.event != nil {
				knownEvents[msg.event.UID] = *msg.event
				// Emit the event message once per Count increment.
				key := emittedKey{uid: msg.event.UID, count: msg.event.Count}
				if !emitted[key] {
					emitted[key] = true
					pe := classifyEvent(msg.event, podMaxContainerRestarts(currentPod), pw.CrashLoopThreshold)
					pw.log.Debug().
						Str("reason", pe.Reason).
						Str("object", pe.ObjectName).
						Str("severity", string(pe.Severity)).
						Str("message", pe.Message).
						Msg("new pod/PVC event")
					pw.emitEvent(opts, pe)
				}
			}

			snap := AnalyzePod(currentPod, eventSlice(), pw.CrashLoopThreshold)
			if snap.Stage == StageRunning && !snap.Created.IsZero() {
				snap.Message = fmt.Sprintf("Workspace is ready, provisioned in %s",
					time.Since(snap.Created).Round(time.Second))
			}

			// Manage the pulling delay timer.
			if snap.Stage == StagePulling {
				if pullingStart.IsZero() {
					pullingStart = time.Now()
					pw.log.Debug().Str("pod", pw.podName).Msg("image pull started; applying delay before reporting pulling status")
				}
			} else {
				pullingStart = time.Time{}
				pullingReported = false
			}

			// Emit status changes, with a delay before reporting StagePulling.
			if snap.Status != lastStatus {
				pw.log.Info().
					Str("pod", pw.podName).
					Str("oldStatus", string(lastStatus)).
					Str("newStatus", string(snap.Status)).
					Str("stage", string(snap.Stage)).
					Str("message", snap.Message).
					Int32("restarts", snap.Restarts).
					Msg("pod status changed")
				if snap.Stage != StagePulling {
					pw.emitSnapshot(opts, snap)
				}
				lastStatus = snap.Status
			}
			if snap.Stage == StagePulling && !pullingReported &&
				!pullingStart.IsZero() && time.Since(pullingStart) >= pullReportDelay {
				pw.log.Debug().
					Str("pod", pw.podName).
					Dur("elapsed", time.Since(pullingStart)).
					Msg("pull delay elapsed; reporting pulling status")
				pw.emitSnapshot(opts, snap)
				pullingReported = true
			}

			// Check terminal conditions.
			if snap.CriticalErr != nil {
				pw.log.Info().
					Err(snap.CriticalErr).
					Str("pod", pw.podName).
					Str("stage", string(snap.Stage)).
					Int32("restarts", snap.Restarts).
					Msg("PodWatcher watchLoop exiting: critical error")
				return &snap, snap.CriticalErr
			}
			switch snap.Stage {
			case StageRunning:
				pw.log.Info().
					Str("pod", pw.podName).
					Int32("restarts", snap.Restarts).
					Msg("PodWatcher watchLoop exiting: pod running")
				return &snap, nil
			case StageStopped, StageFailed:
				pw.log.Info().
					Str("pod", pw.podName).
					Str("stage", string(snap.Stage)).
					Str("message", snap.Message).
					Msg("PodWatcher watchLoop exiting: terminal stage")
				return &snap, fmt.Errorf("workspace %s failed to start: %s", pw.podName, snap.Message)
			}
		}
	}
}

// listEventSources fetches the current event lists for pod and PVC events separately.
// Returns empty (non-nil) lists on error, logging warnings.
func (pw *PodWatcher) listEventSources(ctx context.Context, podName string, pvcNames map[string]bool) (*corev1.EventList, *corev1.EventList) {
	v1 := pw.kubeClient.CoreV1()

	podEvents, err := v1.Events(pw.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.kind=Pod,involvedObject.name=%s", podName),
	})
	if err != nil {
		pw.log.Warn().Err(err).Msg("Failed to list pod events")
		podEvents = &corev1.EventList{}
	}

	pvcEvents := &corev1.EventList{}
	if len(pvcNames) > 0 {
		pvcEvents, err = v1.Events(pw.namespace).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.kind=PersistentVolumeClaim",
		})
		if err != nil {
			pw.log.Warn().Err(err).Msg("Failed to list PVC events")
			pvcEvents = &corev1.EventList{}
		}
	}

	return podEvents, pvcEvents
}

// emitSnapshot sends a status event to opts.Messages if configured.
func (pw *PodWatcher) emitSnapshot(opts *ProvisionOptions, snap PodStateSnapshot) {
	if opts == nil || opts.Messages == nil {
		return
	}
	opts.Messages <- models.WorkspaceStreamEvent{
		Type:       models.WorkspaceStreamEventTypeStatus,
		Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
		ObjectName: pw.podName,
		Status:     snap.Status,
		Message:    snap.Message,
	}
}

// emitEvent sends a pod/PVC event to opts.Messages if configured.
func (pw *PodWatcher) emitEvent(opts *ProvisionOptions, pe PodEvent) {
	if opts == nil || opts.Messages == nil {
		return
	}
	opts.Messages <- models.WorkspaceStreamEvent{
		Type:       models.WorkspaceStreamEventTypeEvent,
		Timestamp:  pe.Time.Format("2006-01-02 15:04:05"),
		ObjectName: fmt.Sprintf("%s/%s", pe.ObjectKind, pe.ObjectName),
		Message:    fmt.Sprintf("%s: %s", pe.Reason, pe.Message),
	}
}
