/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

This file was copied and modified from the kubernetes/kubernetes project
https://github.com/kubernetes/kubernetes/release-1.8/pkg/controller/controller_utils.go

Modifications Copyright SAP SE or an SAP affiliate company and Gardener contributors
*/

// Package controller is used to provide the core functionalities of machine-controller-manager
package controller

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	machineapi "github.com/gardener/machine-controller-manager/pkg/client/clientset/versioned/typed/machine/v1alpha1"
	fakemachineapi "github.com/gardener/machine-controller-manager/pkg/client/clientset/versioned/typed/machine/v1alpha1/fake"
	annotationsutils "github.com/gardener/machine-controller-manager/pkg/util/annotations"
	hashutil "github.com/gardener/machine-controller-manager/pkg/util/hash"
	"github.com/google/uuid"

	"github.com/gardener/machine-controller-manager/pkg/util/provider/machineutils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	clientretry "k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const (
	// ExpectationsTimeout - If a watch drops a delete event for a machine, it'll take this long
	// before a dormant controller waiting for those packets is woken up anyway. It is
	// specifically targeted at the case where some problem prevents an update
	// of expectations, without it the controller could stay asleep forever. This should
	// be set based on the expected latency of watch events.
	//
	// Currently a controller can service (create *and* observe the watch events for said
	// creation) about 10 machines a second, so it takes about 1 min to service
	// 500 machines. Just creation is limited to 20qps, and watching happens with ~10-30s
	// latency/machine at the scale of 3000 machines over 100 nodes.
	ExpectationsTimeout = 5 * time.Minute
	// SlowStartInitialBatchSize - When batching machine creates, is the size of the
	// initial batch.  The size of each successive batch is twice the size of
	// the previous batch.  For example, for a value of 1, batch sizes would be
	// 1, 2, 4, 8, ...  and for a value of 10, batch sizes would be
	// 10, 20, 40, 80, ...  Setting the value higher means that quota denials
	// will result in more doomed API calls and associated event spam.  Setting
	// the value lower will result in more API call round trip periods for
	// large batches.
	//
	// Given a number of machines to start "N":
	// The number of doomed calls per sync once quota is exceeded is given by:
	//      min(N,SlowStartInitialBatchSize)
	// The number of batches is given by:
	//      1+floor(log_2(ceil(N/SlowStartInitialBatchSize)))
	SlowStartInitialBatchSize = 1
)

// Backoff is the backoff period used while updating nodes
var Backoff = wait.Backoff{
	Steps:    5,
	Duration: 100 * time.Millisecond,
	Jitter:   1.0,
}

// UpdateAnnotationBackoff is the backoff period used while updating the annotation
var UpdateAnnotationBackoff = wait.Backoff{
	Steps:    5,
	Duration: 100 * time.Millisecond,
	Jitter:   1.0,
}

var (
	// KeyFunc is the variable that stores the function that retreives the object key from an object
	KeyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
)

// ResyncPeriodFunc is the function that returns the resync duration
type ResyncPeriodFunc func() time.Duration

// NoResyncPeriodFunc Returns 0 for resyncPeriod in case resyncing is not needed.
func NoResyncPeriodFunc() time.Duration {
	return 0
}

// Expectations are a way for controllers to tell the controller manager what they expect. eg:
//	ContExpectations: {
//		controller1: expects  2 adds in 2 minutes
//		controller2: expects  2 dels in 2 minutes
//		controller3: expects -1 adds in 2 minutes => controller3's expectations have already been met
//	}
//
// Implementation:
//	ControlleeExpectation = pair of atomic counters to track controllee's creation/deletion
//	ContExpectationsStore = TTLStore + a ControlleeExpectation per controller
//
// * Once set expectations can only be lowered
// * A controller isn't synced till its expectations are either fulfilled, or expire
// * Controllers that don't set expectations will get woken up for every matching controllee

// ExpKeyFunc to parse out the key from a ControlleeExpectation
var ExpKeyFunc = func(obj interface{}) (string, error) {
	if e, ok := obj.(*ControlleeExpectations); ok {
		return e.key, nil
	}
	return "", fmt.Errorf("Could not find key for obj %#v", obj)
}

// ExpectationsInterface is an interface that allows users to set and wait on expectations.
// Only abstracted out for testing.
// Warning: if using KeyFunc it is not safe to use a single ExpectationsInterface with different
// types of controllers, because the keys might conflict across types.
type ExpectationsInterface interface {
	GetExpectations(controllerKey string) (*ControlleeExpectations, bool, error)
	SatisfiedExpectations(controllerKey string) bool
	DeleteExpectations(controllerKey string)
	SetExpectations(controllerKey string, add, del int) error
	ExpectCreations(controllerKey string, adds int) error
	ExpectDeletions(controllerKey string, dels int) error
	CreationObserved(controllerKey string)
	DeletionObserved(controllerKey string)
	RaiseExpectations(controllerKey string, add, del int)
	LowerExpectations(controllerKey string, add, del int)
}

// ContExpectations is a cache mapping controllers to what they expect to see before being woken up for a sync.
type ContExpectations struct {
	cache.Store
}

// GetExpectations returns the ControlleeExpectations of the given controller.
func (r *ContExpectations) GetExpectations(controllerKey string) (*ControlleeExpectations, bool, error) {
	var err error
	if exp, exists, err := r.GetByKey(controllerKey); err == nil && exists {
		return exp.(*ControlleeExpectations), true, nil
	}

	return nil, false, err
}

// DeleteExpectations deletes the expectations of the given controller from the TTLStore.
func (r *ContExpectations) DeleteExpectations(controllerKey string) {
	if exp, exists, err := r.GetByKey(controllerKey); err == nil && exists {
		if err := r.Delete(exp); err != nil {
			klog.V(4).Infof("Error deleting expectations for controller %v: %v", controllerKey, err)
		}
	}
}

// SatisfiedExpectations returns true if the required adds/dels for the given controller have been observed.
// Add/del counts are established by the controller at sync time, and updated as controllees are observed by the controller
// manager.
func (r *ContExpectations) SatisfiedExpectations(controllerKey string) bool {
	if exp, exists, err := r.GetExpectations(controllerKey); exists {
		if exp.Fulfilled() {
			klog.V(4).Infof("Controller expectations fulfilled %#v", exp)
			return true
		} else if exp.isExpired() {
			klog.V(4).Infof("Controller expectations expired %#v", exp)
			return true
		} else {
			klog.V(4).Infof("Controller still waiting on expectations %#v", exp)
			return false
		}
	} else if err != nil {
		klog.V(2).Infof("Error encountered while checking expectations %#v, forcing sync", err)
	} else {
		// When a new controller is created, it doesn't have expectations.
		// When it doesn't see expected watch events for > TTL, the expectations expire.
		//	- In this case it wakes up, creates/deletes controllees, and sets expectations again.
		// When it has satisfied expectations and no controllees need to be created/destroyed > TTL, the expectations expire.
		//	- In this case it continues without setting expectations till it needs to create/delete controllees.
		klog.V(4).Infof("Controller %v either never recorded expectations, or the ttl expired.", controllerKey)
	}
	// Trigger a sync if we either encountered and error (which shouldn't happen since we're
	// getting from local store) or this controller hasn't established expectations.
	return true
}

// TODO: Extend ExpirationCache to support explicit expiration.
// TODO: Make this possible to disable in tests.
// TODO: Support injection of clock.
func (exp *ControlleeExpectations) isExpired() bool {
	return clock.RealClock{}.Since(exp.timestamp) > ExpectationsTimeout
}

// SetExpectations registers new expectations for the given controller. Forgets existing expectations.
func (r *ContExpectations) SetExpectations(controllerKey string, add, del int) error {
	exp := &ControlleeExpectations{add: int64(add), del: int64(del), key: controllerKey, timestamp: clock.RealClock{}.Now()}
	klog.V(4).Infof("Setting expectations %#v", exp)
	return r.Add(exp)
}

// ExpectCreations adds creations to an existing expectation
func (r *ContExpectations) ExpectCreations(controllerKey string, adds int) error {
	return r.SetExpectations(controllerKey, adds, 0)
}

// ExpectDeletions deletion creations to an existing expectation
func (r *ContExpectations) ExpectDeletions(controllerKey string, dels int) error {
	return r.SetExpectations(controllerKey, 0, dels)
}

// LowerExpectations Decrements the expectation counts of the given controller.
func (r *ContExpectations) LowerExpectations(controllerKey string, add, del int) {
	if exp, exists, err := r.GetExpectations(controllerKey); err == nil && exists {
		exp.Add(int64(-add), int64(-del))
		// The expectations might've been modified since the update on the previous line.
		klog.V(4).Infof("Lowered expectations %#v", exp)
	}
}

// RaiseExpectations Increments the expectation counts of the given controller.
func (r *ContExpectations) RaiseExpectations(controllerKey string, add, del int) {
	if exp, exists, err := r.GetExpectations(controllerKey); err == nil && exists {
		exp.Add(int64(add), int64(del))
		// The expectations might've been modified since the update on the previous line.
		klog.V(4).Infof("Raised expectations %#v", exp)
	}
}

// CreationObserved atomically decrements the `add` expectation count of the given controller.
func (r *ContExpectations) CreationObserved(controllerKey string) {
	r.LowerExpectations(controllerKey, 1, 0)
}

// DeletionObserved atomically decrements the `del` expectation count of the given controller.
func (r *ContExpectations) DeletionObserved(controllerKey string) {
	r.LowerExpectations(controllerKey, 0, 1)
}

// Expectations are either fulfilled, or expire naturally.
type Expectations interface {
	Fulfilled() bool
}

// ControlleeExpectations track controllee creates/deletes.
type ControlleeExpectations struct {
	// Important: Since these two int64 fields are using sync/atomic, they have to be at the top of the struct due to a bug on 32-bit platforms
	// See: https://golang.org/pkg/sync/atomic/ for more information
	add       int64
	del       int64
	key       string
	timestamp time.Time
}

// Add increments the add and del counters.
func (exp *ControlleeExpectations) Add(add, del int64) {
	atomic.AddInt64(&exp.add, add)
	atomic.AddInt64(&exp.del, del)
}

// Fulfilled returns true if this expectation has been fulfilled.
func (exp *ControlleeExpectations) Fulfilled() bool {
	// TODO: think about why this line being atomic doesn't matter
	return atomic.LoadInt64(&exp.add) <= 0 && atomic.LoadInt64(&exp.del) <= 0
}

// GetExpectations returns the add and del expectations of the controllee.
func (exp *ControlleeExpectations) GetExpectations() (int64, int64) {
	return atomic.LoadInt64(&exp.add), atomic.LoadInt64(&exp.del)
}

// NewContExpectations returns a store for ContExpectations.
func NewContExpectations() *ContExpectations {
	return &ContExpectations{cache.NewStore(ExpKeyFunc)}
}

// UIDSetKeyFunc to parse out the key from a UIDSet.
var UIDSetKeyFunc = func(obj interface{}) (string, error) {
	if u, ok := obj.(*UIDSet); ok {
		return u.key, nil
	}
	return "", fmt.Errorf("Could not find key for obj %#v", obj)
}

// UIDSet holds a key and a set of UIDs. Used by the
// UIDTrackingContExpectations to remember which UID it has seen/still
// waiting for.
type UIDSet struct {
	sets.String
	key string
}

// UIDTrackingContExpectations tracks the UID of the machines it deletes.
// This cache is needed over plain old expectations to safely handle graceful
// deletion. The desired behavior is to treat an update that sets the
// DeletionTimestamp on an object as a delete. To do so consistently, one needs
// to remember the expected deletes so they aren't double counted.
// TODO: Track creates as well (#22599)
type UIDTrackingContExpectations struct {
	ExpectationsInterface
	// TODO: There is a much nicer way to do this that involves a single store,
	// a lock per entry, and a ControlleeExpectationsInterface type.
	uidStoreLock sync.Mutex
	// Store used for the UIDs associated with any expectation tracked via the
	// ExpectationsInterface.
	uidStore cache.Store
}

// GetUIDs is a convenience method to avoid exposing the set of expected uids.
// The returned set is not thread safe, all modifications must be made holding
// the uidStoreLock.
func (u *UIDTrackingContExpectations) GetUIDs(controllerKey string) sets.String {
	if uid, exists, err := u.uidStore.GetByKey(controllerKey); err == nil && exists {
		return uid.(*UIDSet).String
	}
	return nil
}

// ExpectDeletions records expectations for the given deleteKeys, against the given controller.
func (u *UIDTrackingContExpectations) ExpectDeletions(rcKey string, deletedKeys []string) error {
	u.uidStoreLock.Lock()
	defer u.uidStoreLock.Unlock()

	if existing := u.GetUIDs(rcKey); existing != nil && existing.Len() != 0 {
		klog.Errorf("Clobbering existing delete keys: %+v", existing)
	}
	expectedUIDs := sets.NewString()
	for _, k := range deletedKeys {
		expectedUIDs.Insert(k)
	}
	klog.V(4).Infof("Controller %v waiting on deletions for: %+v", rcKey, deletedKeys)
	if err := u.uidStore.Add(&UIDSet{expectedUIDs, rcKey}); err != nil {
		return err
	}
	return u.ExpectationsInterface.ExpectDeletions(rcKey, expectedUIDs.Len())
}

// DeletionObserved records the given deleteKey as a deletion, for the given rc.
func (u *UIDTrackingContExpectations) DeletionObserved(rcKey, deleteKey string) {
	u.uidStoreLock.Lock()
	defer u.uidStoreLock.Unlock()

	uids := u.GetUIDs(rcKey)
	if uids != nil && uids.Has(deleteKey) {
		klog.V(3).Infof("Controller %v received delete for machine %v", rcKey, deleteKey)
		u.ExpectationsInterface.DeletionObserved(rcKey)
		uids.Delete(deleteKey)
	}
}

// DeleteExpectations deletes the UID set and invokes DeleteExpectations on the
// underlying ExpectationsInterface.
func (u *UIDTrackingContExpectations) DeleteExpectations(rcKey string) {
	u.uidStoreLock.Lock()
	defer u.uidStoreLock.Unlock()

	u.ExpectationsInterface.DeleteExpectations(rcKey)
	if uidExp, exists, err := u.uidStore.GetByKey(rcKey); err == nil && exists {
		if err := u.uidStore.Delete(uidExp); err != nil {
			klog.V(2).Infof("Error deleting uid expectations for controller %v: %v", rcKey, err)
		}
	}
}

// NewUIDTrackingContExpectations returns a wrapper around
// ContExpectations that is aware of deleteKeys.
func NewUIDTrackingContExpectations(ce ExpectationsInterface) *UIDTrackingContExpectations {
	return &UIDTrackingContExpectations{ExpectationsInterface: ce, uidStore: cache.NewStore(UIDSetKeyFunc)}
}

// Reasons for machine events
const (
	// FailedCreateMachineReason is added in an event and in a machine set condition
	// when a machine for a machine set is failed to be created.
	FailedCreateMachineReason = "FailedCreate"
	// SuccessfulCreateMachineReason is added in an event when a machine for a machine set
	// is successfully created.
	SuccessfulCreateMachineReason = "SuccessfulCreate"
	// FailedDeleteMachineReason is added in an event and in a machine set condition
	// when a machine for a machine set is failed to be deleted.
	FailedDeleteMachineReason = "FailedDelete"
	// SuccessfulDeletemachineReason is added in an event when a machine for a machine set
	// is successfully deleted.
	SuccessfulDeleteMachineReason = "SuccessfulDelete"
)

// MachineSetControlInterface is an interface that knows how to add or delete
// MachineSets, as well as increment or decrement them. It is used
// by the deployment controller to ease testing of actions that it takes.
type MachineSetControlInterface interface {
	PatchMachineSet(ctx context.Context, namespace, name string, data []byte) error
}

// RealMachineSetControl is the default implementation of RSControllerInterface.
type RealMachineSetControl struct {
	controlMachineClient machineapi.MachineV1alpha1Interface
	Recorder             record.EventRecorder
}

var _ MachineSetControlInterface = &RealMachineSetControl{}

// PatchMachineSet patches the machineSet object
func (r RealMachineSetControl) PatchMachineSet(ctx context.Context, namespace, name string, data []byte) error {
	_, err := r.controlMachineClient.MachineSets(namespace).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	return err
}

// RevisionControlInterface is an interface that knows how to patch
// ControllerRevisions, as well as increment or decrement them. It is used
// by the daemonset controller to ease testing of actions that it takes.
// TODO: merge the controller revision interface in controller_history.go with this one
type RevisionControlInterface interface {
	PatchControllerRevision(ctx context.Context, namespace, name string, data []byte) error
}

// RealControllerRevisionControl is the default implementation of RevisionControlInterface.
type RealControllerRevisionControl struct {
	KubeClient clientset.Interface
}

var _ RevisionControlInterface = &RealControllerRevisionControl{}

// PatchControllerRevision is the patch method used to patch the controller revision
func (r RealControllerRevisionControl) PatchControllerRevision(ctx context.Context, namespace, name string, data []byte) error {
	_, err := r.KubeClient.AppsV1beta1().ControllerRevisions(namespace).Patch(ctx, name, types.StrategicMergePatchType, data, metav1.PatchOptions{})
	return err
}

func validateControllerRef(controllerRef *metav1.OwnerReference) error {
	if controllerRef == nil {
		return fmt.Errorf("controllerRef is nil")
	}
	if len(controllerRef.APIVersion) == 0 {
		return fmt.Errorf("controllerRef has empty APIVersion")
	}
	if len(controllerRef.Kind) == 0 {
		return fmt.Errorf("controllerRef has empty Kind")
	}
	if controllerRef.Controller == nil || !*controllerRef.Controller {
		return fmt.Errorf("controllerRef.Controller is not set to true")
	}
	if controllerRef.BlockOwnerDeletion == nil || !*controllerRef.BlockOwnerDeletion {
		return fmt.Errorf("controllerRef.BlockOwnerDeletion is not set")
	}
	return nil
}

//--- For Machines ---//

// RealMachineControl is the default implementation of machineControlInterface.
type RealMachineControl struct {
	controlMachineClient machineapi.MachineV1alpha1Interface
	Recorder             record.EventRecorder
}

// MachineControlInterface is the reference to the realMachineControl
var _ MachineControlInterface = &RealMachineControl{}

// MachineControlInterface is the interface used by the machine-set controller to interact with the machine controller
type MachineControlInterface interface {
	// Createmachines creates new machines according to the spec.
	CreateMachines(ctx context.Context, namespace string, template *v1alpha1.MachineTemplateSpec, object runtime.Object) error
	// CreatemachinesWithControllerRef creates new machines according to the spec, and sets object as the machine's controller.
	CreateMachinesWithControllerRef(ctx context.Context, namespace string, template *v1alpha1.MachineTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference) error
	// Deletemachine deletes the machine identified by machineID.
	DeleteMachine(ctx context.Context, namespace string, machineID string, object runtime.Object) error
	// Patchmachine patches the machine.
	PatchMachine(ctx context.Context, namespace string, name string, data []byte) error
}

func getMachinesLabelSet(template *v1alpha1.MachineTemplateSpec) labels.Set {
	desiredLabels := make(labels.Set)
	for k, v := range template.Labels {
		desiredLabels[k] = v
	}
	return desiredLabels
}

func getMachinesFinalizers(template *v1alpha1.MachineTemplateSpec) []string {
	desiredFinalizers := make([]string, len(template.Finalizers))
	copy(desiredFinalizers, template.Finalizers)
	return desiredFinalizers
}

func getMachinesAnnotationSet(template *v1alpha1.MachineTemplateSpec, _ runtime.Object) labels.Set {
	desiredAnnotations := make(labels.Set)
	for k, v := range template.Annotations {
		desiredAnnotations[k] = v
	}
	return desiredAnnotations
}

func getMachinesPrefix(controllerName string) string {
	// use the dash (if the name isn't too long) to make the machine name a bit prettier
	prefix := fmt.Sprintf("%s-", controllerName)
	if len(validation.NameIsDNSSubdomain(prefix, true)) != 0 { // #ToCheck
		prefix = controllerName
	}
	return prefix
}

// CreateMachinesWithControllerRef creates a machine with controller reference
func (r RealMachineControl) CreateMachinesWithControllerRef(ctx context.Context, namespace string, template *v1alpha1.MachineTemplateSpec, controllerObject runtime.Object, controllerRef *metav1.OwnerReference) error {
	if err := validateControllerRef(controllerRef); err != nil {
		return err
	}
	return r.createMachines(ctx, namespace, template, controllerObject, controllerRef)
}

// GetMachineFromTemplate passes the machine template spec to return the machine object
func GetMachineFromTemplate(template *v1alpha1.MachineTemplateSpec, parentObject runtime.Object, controllerRef *metav1.OwnerReference) (*v1alpha1.Machine, error) {

	//klog.Info("Template details \n", template.Spec.Class)
	desiredLabels := getMachinesLabelSet(template)
	//klog.Info(desiredLabels)
	desiredFinalizers := getMachinesFinalizers(template)
	desiredAnnotations := getMachinesAnnotationSet(template, parentObject)

	accessor, err := meta.Accessor(parentObject)
	if err != nil {
		return nil, fmt.Errorf("parentObject does not have ObjectMeta, %v", err)
	}
	prefix := getMachinesPrefix(accessor.GetName())

	machine := &v1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Labels:       desiredLabels,
			Annotations:  desiredAnnotations,
			GenerateName: prefix,
			Finalizers:   desiredFinalizers,
		},
		Spec: v1alpha1.MachineSpec{
			Class: template.Spec.Class,
		},
	}
	if controllerRef != nil {
		machine.OwnerReferences = append(machine.OwnerReferences, *controllerRef)
	}
	machine.Spec = *template.Spec.DeepCopy()

	return machine, nil
}

// CreateMachines initiates a create machine for a RealMachineControl
func (r RealMachineControl) CreateMachines(ctx context.Context, namespace string, template *v1alpha1.MachineTemplateSpec, object runtime.Object) error {
	return r.createMachines(ctx, namespace, template, object, nil)
}

func (r RealMachineControl) createMachines(ctx context.Context, namespace string, template *v1alpha1.MachineTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference) error {
	machine, err := GetMachineFromTemplate(template, object, controllerRef)
	if err != nil {
		return err
	}

	if labels.Set(machine.Labels).AsSelectorPreValidated().Empty() {
		return fmt.Errorf("unable to create machines, no labels")
	}

	var newMachine *v1alpha1.Machine
	if newMachine, err = r.controlMachineClient.Machines(namespace).Create(ctx, machine, metav1.CreateOptions{}); err != nil {
		klog.Error(err)
		r.Recorder.Eventf(object, v1.EventTypeWarning, FailedCreateMachineReason, "Error creating: %v", err)
		return err
	}
	accessor, err := meta.Accessor(object)
	if err != nil {
		klog.Errorf("parentObject does not have ObjectMeta, %v", err)
		return nil
	}

	klog.V(3).Infof("Controller %v created machine %v", accessor.GetName(), newMachine.Name)
	r.Recorder.Eventf(object, v1.EventTypeNormal, SuccessfulCreateMachineReason, "Created Machine: %v", newMachine.Name)

	return nil
}

// PatchMachine applies a patch on machine
func (r RealMachineControl) PatchMachine(ctx context.Context, namespace string, name string, data []byte) error {
	_, err := r.controlMachineClient.Machines(namespace).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	return err
}

// DeleteMachine deletes a machine attached to the RealMachineControl
func (r RealMachineControl) DeleteMachine(ctx context.Context, namespace string, machineID string, object runtime.Object) error {
	accessor, err := meta.Accessor(object)
	if err != nil {
		return fmt.Errorf("object does not have ObjectMeta, %v", err)
	}
	klog.V(3).Infof("Controller %v deleting machine %v", accessor.GetName(), machineID)

	if err := r.controlMachineClient.Machines(namespace).Delete(ctx, machineID, metav1.DeleteOptions{}); err != nil {
		r.Recorder.Eventf(object, v1.EventTypeWarning, FailedDeleteMachineReason, "Error deleting: %v", err)
		return fmt.Errorf("unable to delete machines: %v", err)
	}
	r.Recorder.Eventf(object, v1.EventTypeNormal, SuccessfulDeleteMachineReason, "Deleted machine: %v", machineID)

	return nil
}

// --- //

// -- Fake Machine Control -- //

// FakeMachineControl is the fake implementation of machineControlInterface.
type FakeMachineControl struct {
	controlMachineClient *fakemachineapi.FakeMachineV1alpha1
	Recorder             record.EventRecorder
}

// CreateMachines initiates a create machine for a RealMachineControl
func (r FakeMachineControl) CreateMachines(ctx context.Context, namespace string, template *v1alpha1.MachineTemplateSpec, object runtime.Object) error {
	return r.createMachines(ctx, namespace, template, object, nil)
}

func (r FakeMachineControl) createMachines(ctx context.Context, namespace string, template *v1alpha1.MachineTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference) error {
	machine, err := GetFakeMachineFromTemplate(template, object, controllerRef)
	if err != nil {
		return err
	}

	if labels.Set(machine.Labels).AsSelectorPreValidated().Empty() {
		return fmt.Errorf("unable to create machines, no labels")
	}

	var newMachine *v1alpha1.Machine
	if newMachine, err = r.controlMachineClient.Machines(namespace).Create(ctx, machine, metav1.CreateOptions{}); err != nil {
		klog.Error(err)
		r.Recorder.Eventf(object, v1.EventTypeWarning, FailedCreateMachineReason, "Error creating: %v", err)
		return err
	}
	accessor, err := meta.Accessor(object)
	if err != nil {
		klog.Errorf("parentObject does not have ObjectMeta, %v", err)
		return nil
	}

	klog.V(2).Infof("Controller %v created machine %v", accessor.GetName(), newMachine.Name)

	return nil
}

// CreateMachinesWithControllerRef creates a machine with controller reference
func (r FakeMachineControl) CreateMachinesWithControllerRef(ctx context.Context, namespace string, template *v1alpha1.MachineTemplateSpec, controllerObject runtime.Object, controllerRef *metav1.OwnerReference) error {
	if err := validateControllerRef(controllerRef); err != nil {
		return err
	}
	return r.createMachines(ctx, namespace, template, controllerObject, controllerRef)
}

// PatchMachine applies a patch on machine
func (r FakeMachineControl) PatchMachine(ctx context.Context, namespace string, name string, data []byte) error {
	_, err := r.controlMachineClient.Machines(namespace).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	return err
}

// DeleteMachine deletes a machine attached to the RealMachineControl
func (r FakeMachineControl) DeleteMachine(ctx context.Context, namespace string, machineID string, object runtime.Object) error {
	accessor, err := meta.Accessor(object)
	if err != nil {
		return fmt.Errorf("object does not have ObjectMeta, %v", err)
	}
	klog.V(2).Infof("Controller %v deleting machine %v", accessor.GetName(), machineID)

	if err := r.controlMachineClient.Machines(namespace).Delete(ctx, machineID, metav1.DeleteOptions{}); err != nil {
		r.Recorder.Eventf(object, v1.EventTypeWarning, FailedDeleteMachineReason, "Error deleting: %v", err)
		return fmt.Errorf("unable to delete machines: %v", err)
	}

	return nil
}

// GetFakeMachineFromTemplate passes the machine template spec to return the machine object
func GetFakeMachineFromTemplate(template *v1alpha1.MachineTemplateSpec, parentObject runtime.Object, controllerRef *metav1.OwnerReference) (*v1alpha1.Machine, error) {

	desiredLabels := getMachinesLabelSet(template)

	desiredFinalizers := getMachinesFinalizers(template)
	desiredAnnotations := getMachinesAnnotationSet(template, parentObject)

	accessor, err := meta.Accessor(parentObject)
	if err != nil {
		return nil, fmt.Errorf("parentObject does not have ObjectMeta, %v", err)
	}
	prefix := getMachinesPrefix(accessor.GetName())
	prefix = prefix + "-" + uuid.New().String()[:5]
	machine := &v1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      desiredLabels,
			Annotations: desiredAnnotations,
			Name:        prefix,
			Finalizers:  desiredFinalizers,
		},
		Spec: v1alpha1.MachineSpec{
			Class: template.Spec.Class,
		},
	}
	if controllerRef != nil {
		machine.OwnerReferences = append(machine.OwnerReferences, *controllerRef)
	}
	machine.Spec = *template.Spec.DeepCopy()

	return machine, nil
}

// --- //

// ActiveMachines type allows custom sorting of machines so a controller can pick the best ones to delete.
type ActiveMachines []*v1alpha1.Machine

func (s ActiveMachines) Len() int      { return len(s) }
func (s ActiveMachines) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s ActiveMachines) Less(i, j int) bool {
	// Default priority for machine objects
	machineIPriority := 3
	machineJPriority := 3

	if s[i].Annotations != nil && s[i].Annotations[machineutils.MachinePriority] != "" {
		num, err := strconv.Atoi(s[i].Annotations[machineutils.MachinePriority])
		if err == nil {
			machineIPriority = num
		} else {
			klog.Errorf("Machine priority is taken to be the default value (3). Couldn't convert machine priority to integer for machine:%s. Error message - %s", s[i].Name, err)
		}
	}

	if s[j].Annotations != nil && s[j].Annotations[machineutils.MachinePriority] != "" {
		num, err := strconv.Atoi(s[j].Annotations[machineutils.MachinePriority])
		if err == nil {
			machineJPriority = num
		} else {
			klog.Errorf("Machine priority is taken to be the default value (3). Couldn't convert machine priority to integer for machine:%s. Error message - %s", s[j].Name, err)
		}
	}

	// Map containing machinePhase priority
	// the lower the priority, the more likely
	// it is to be deleted
	m := map[v1alpha1.MachinePhase]int{
		v1alpha1.MachineTerminating:      0,
		v1alpha1.MachineFailed:           1,
		v1alpha1.MachineCrashLoopBackOff: 2,
		v1alpha1.MachineUnknown:          3,
		v1alpha1.MachinePending:          4,
		v1alpha1.MachineAvailable:        5,
		v1alpha1.MachineRunning:          6,
	}

	// Case-1: Initially we try to prioritize machine deletion based on
	// machinePriority annotation.
	// Case-2: If both priorities are equal, then we look at their machinePhase
	// and prioritize as mentioned in the above map
	// Case-3: If both Case-1 & Case-2 is false, we prioritize based on creation time
	if machineIPriority != machineJPriority {
		return machineIPriority < machineJPriority
	} else if m[s[i].Status.CurrentStatus.Phase] != m[s[j].Status.CurrentStatus.Phase] {
		return m[s[i].Status.CurrentStatus.Phase] < m[s[j].Status.CurrentStatus.Phase]
	} else if s[i].CreationTimestamp != s[j].CreationTimestamp {
		return s[i].CreationTimestamp.Before(&s[j].CreationTimestamp)
	}

	return false
}

// MachineKey is the function used to get the machine name from machine object
// ToCheck : as machine-namespace does not matter
func MachineKey(machine *v1alpha1.Machine) string {
	return fmt.Sprintf("%v", machine.Name)
}

// ControllersByCreationTimestamp sorts a list of ReplicationControllers by creation timestamp, using their names as a tie breaker.
type ControllersByCreationTimestamp []*v1.ReplicationController

func (o ControllersByCreationTimestamp) Len() int      { return len(o) }
func (o ControllersByCreationTimestamp) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o ControllersByCreationTimestamp) Less(i, j int) bool {
	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}

// MachineSetsByCreationTimestamp sorts a list of MachineSet by creation timestamp, using their names as a tie breaker.
/****************** For MachineSet **********************/
type MachineSetsByCreationTimestamp []*v1alpha1.MachineSet

func (o MachineSetsByCreationTimestamp) Len() int      { return int(len(o)) }
func (o MachineSetsByCreationTimestamp) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o MachineSetsByCreationTimestamp) Less(i, j int) bool {
	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}

// MachineSetsBySizeOlder sorts a list of MachineSet by size in descending order, using their creation timestamp or name as a tie breaker.
// By using the creation timestamp, this sorts from old to new machine sets.
type MachineSetsBySizeOlder []*v1alpha1.MachineSet

func (o MachineSetsBySizeOlder) Len() int      { return int(len(o)) }
func (o MachineSetsBySizeOlder) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o MachineSetsBySizeOlder) Less(i, j int) bool {
	if (o[i].Spec.Replicas) == (o[j].Spec.Replicas) {
		return MachineSetsByCreationTimestamp(o).Less(int(i), int(j))
	}
	return (o[i].Spec.Replicas) > (o[j].Spec.Replicas)
}

// MachineSetsBySizeNewer sorts a list of MachineSet by size in descending order, using their creation timestamp or name as a tie breaker.
// By using the creation timestamp, this sorts from new to old machine sets.
type MachineSetsBySizeNewer []*v1alpha1.MachineSet

func (o MachineSetsBySizeNewer) Len() int      { return int(len(o)) }
func (o MachineSetsBySizeNewer) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o MachineSetsBySizeNewer) Less(i, j int) bool {
	if (o[i].Spec.Replicas) == (o[j].Spec.Replicas) {
		return MachineSetsByCreationTimestamp(o).Less(j, i)
	}
	return (o[i].Spec.Replicas) > (o[j].Spec.Replicas)
}

// FilterActiveMachineSets returns machine sets that have (or at least ought to have) machines.
func FilterActiveMachineSets(machineSets []*v1alpha1.MachineSet) []*v1alpha1.MachineSet {
	activeFilter := func(is *v1alpha1.MachineSet) bool {
		return is != nil && (is.Spec.Replicas) > 0
	}
	return FilterMachineSets(machineSets, activeFilter)
}

type filterIS func(is *v1alpha1.MachineSet) bool

// FilterMachineSets returns machine sets that are filtered by filterFn (all returned ones should match filterFn).
func FilterMachineSets(ISes []*v1alpha1.MachineSet, filterFn filterIS) []*v1alpha1.MachineSet {
	var filtered []*v1alpha1.MachineSet
	for i := range ISes {
		if filterFn(ISes[i]) {
			filtered = append(filtered, ISes[i])
		}
	}
	return filtered
}

// WaitForCacheSync is a wrapper around cache.WaitForCacheSync that generates log messages
// indicating that the controller identified by controllerName is waiting for syncs, followed by
// either a successful or failed sync.
func WaitForCacheSync(controllerName string, stopCh <-chan struct{}, cacheSyncs ...cache.InformerSynced) bool {
	klog.Infof("Waiting for caches to sync for %s controller", controllerName)

	if !cache.WaitForCacheSync(stopCh, cacheSyncs...) {
		utilruntime.HandleError(fmt.Errorf("Unable to sync caches for %s controller", controllerName))
		return false
	}

	klog.Infof("Caches are synced for %s controller", controllerName)
	return true
}

// ComputeHash returns a hash value calculated from machine template and a collisionCount to avoid hash collision
func ComputeHash(template *v1alpha1.MachineTemplateSpec, collisionCount *int32) uint32 {
	machineTemplateSpecHasher := fnv.New32a()
	hashutil.DeepHashObject(machineTemplateSpecHasher, *template)

	// Add collisionCount in the hash if it exists.
	if collisionCount != nil {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(*collisionCount)) // #nosec G115 (CWE-190) -- collisionCount cannot be negative
		_, err := machineTemplateSpecHasher.Write(collisionCountBytes)
		if err != nil {
			klog.Warningf("Unable to write collision count: %v", err)
		}
	}

	return machineTemplateSpecHasher.Sum32()
}

// AddOrUpdateAnnotationOnNode add annotations to the node. If annotation was added into node, it'll issue API calls
// to update nodes; otherwise, no API calls. Return error if any.
func AddOrUpdateAnnotationOnNode(ctx context.Context, c clientset.Interface, nodeName string, annotations map[string]string) error {
	if annotations == nil {
		return nil
	}
	firstTry := true
	return clientretry.RetryOnConflict(UpdateAnnotationBackoff, func() error {
		var err error
		var oldNode *v1.Node
		// First we try getting node from the API server cache, as it's cheaper. If it fails
		// we get it from etcd to be sure to have fresh data.
		if firstTry {
			oldNode, err = c.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{ResourceVersion: "0"})
			firstTry = false
		} else {
			oldNode, err = c.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		}
		if errors.IsNotFound(err) {
			klog.Warningf("Node %s not found while updating annotation. Err: %v", nodeName, err)
			return nil
		}
		if err != nil {
			return err
		}

		var newNode *v1.Node
		updated := false

		newNode, updated, err = annotationsutils.AddOrUpdateAnnotation(oldNode, annotations)
		if err != nil {
			return err
		}

		if !updated {
			return nil
		}
		return UpdateNodeAnnotations(ctx, c, nodeName, oldNode, newNode)
	})
}

// UpdateNodeAnnotations is for updating the node annotations from oldNode to the newNode
// using the nodes Update() method
func UpdateNodeAnnotations(ctx context.Context, c clientset.Interface, nodeName string, oldNode *v1.Node, newNode *v1.Node) error {
	newNodeClone := oldNode.DeepCopy()
	newNodeClone.Annotations = newNode.Annotations

	_, err := c.CoreV1().Nodes().Update(ctx, newNodeClone, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create or update annotations for node %q: %v", nodeName, err)
	}

	return err
}

// RemoveAnnotationsOffNode is for cleaning up annotations temporarily added to node,
// won't fail if target annotation doesn't exist or has been removed.
// If passed a node it'll check if there's anything to be done, if annotation is not present it won't issue
// any API calls.
func RemoveAnnotationsOffNode(ctx context.Context, c clientset.Interface, nodeName string, annotations map[string]string) error {

	// Short circuit if annotation doesnt exist for limiting API calls.
	if annotations == nil || nodeName == "" {
		return nil
	}

	firstTry := true
	return clientretry.RetryOnConflict(UpdateAnnotationBackoff, func() error {
		var err error
		var oldNode *v1.Node
		// First we try getting node from the API server cache, as it's cheaper. If it fails
		// we get it from etcd to be sure to have fresh data.
		if firstTry {
			oldNode, err = c.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{ResourceVersion: "0"})
			firstTry = false
		} else {
			oldNode, err = c.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		}
		if errors.IsNotFound(err) {
			klog.Warningf("Node %s not found while removing annotation. Err: %v", nodeName, err)
			return nil
		}

		if err != nil {
			return err
		}

		var newNode *v1.Node
		oldNodeCopy := oldNode
		updated := false

		// Remove the annotations from the node.
		newNode, updated, err = annotationsutils.RemoveAnnotation(oldNodeCopy, annotations)
		if err != nil {
			return err
		}

		if !updated {
			return nil
		}
		return UpdateNodeAnnotations(ctx, c, nodeName, oldNode, newNode)
	})
}

// GetAnnotationsFromNode returns all the annotations of the provided node.
func GetAnnotationsFromNode(ctx context.Context, c clientset.Interface, nodeName string) (map[string]string, error) {

	// Short circuit if annotation doesnt exist for limiting API calls.
	if nodeName == "" {
		return nil, nil
	}

	node, err := c.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		klog.Warningf("Node %s not found while fetching annotation. Err: %v", nodeName, err)
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return node.Annotations, nil
}
