package main

import (
	"context"
	"flag"
	"fmt"
	rolloutsv1alpha1 "github.com/openkruise/rollouts/api/v1alpha1"
	"github.com/openkruise/rollouts/cmd/tools"
	deploymentutil "github.com/openkruise/rollouts/pkg/controller/deployment/util"
	"github.com/openkruise/rollouts/pkg/util"
	labelsutil "github.com/openkruise/rollouts/pkg/util/labels"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/integer"
	"os"
	"strconv"
)

var configFile *string
var caseName *string
var ns *string
var workload *string
var image *string
var num *int

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = apps.SchemeGroupVersion.WithKind("Deployment")
var maxRevHistoryLengthInChars = 2000
var strategy = rolloutsv1alpha1.DeploymentStrategy{
	RollingUpdate: &apps.RollingUpdateDeployment{},
}

func init() {
	configFile = flag.String("configFile", "/Users/jacksontong/.kube/cls-g1hrm8bg-config", "file path to kubeconfig")
	caseName = flag.String("caseName", "begin", "case to excute")
	ns = flag.String("ns", "default", "namespace of workload")
	workload = flag.String("workload", "workload-demo3", "name of workload")
	image = flag.String("image", "busybox:1.36", "image of workload")
	num = flag.Int("num", 1, "num of pod to upgrade")

	flag.Parse()

	maxSurge := intstr.FromInt(1)
	maxUnavailable := intstr.FromInt(0)
	strategy.RollingStyle = rolloutsv1alpha1.PartitionRollingStyle
	strategy.Partition = intstr.FromInt(1)
	strategy.RollingUpdate.MaxSurge = &maxSurge
	strategy.RollingUpdate.MaxUnavailable = &maxUnavailable
}

func main() {
	if *configFile == "" || *caseName == "" {
		fmt.Printf("Pls set configFile and caseName")
		os.Exit(-1)
	}

	client, err := tools.LoadsKubeConfigFromFile(*configFile)
	if err != nil {
		fmt.Printf("build client failed: %s %v", *configFile, err)
		return
	}

	d, err := client.AppsV1().Deployments(*ns).Get(context.TODO(), *workload, metav1.GetOptions{})
	if err != nil {
		fmt.Printf("get deploy failed: %v", err)
		return
	}

	switch *caseName {
	case "begin":
		// patch deploy's paused/image
		if *image == "" {
			fmt.Printf("Pls set image")
			return
		}
		err = updateDeployment(client, d, true, *image)
	case "batch":
		// create NewRS if not exist
		// scaler up/down new/old rs
		err = adjustRsForDeployment(client, d)
	case "end":
		// restore deploy's paused
		err = updateDeployment(client, d, false, "")
	}

	fmt.Println("=========================main")
	fmt.Println(err)
	fmt.Println("=========================")
}

func updateDeployment(client *kubernetes.Clientset, d *apps.Deployment, pause bool, image string) error {
	newdp := d.DeepCopy()
	newdp.Spec.Paused = pause
	if pause {
		newdp.Spec.Strategy = apps.DeploymentStrategy{
			Type: apps.RecreateDeploymentStrategyType,
		}
	}
	if image != "" {
		newdp.Spec.Template.Spec.Containers[0].Image = image
	}
	_, err := client.AppsV1().Deployments(newdp.Namespace).Update(context.TODO(), newdp, metav1.UpdateOptions{})
	return err
}

func adjustRsForDeployment(client *kubernetes.Clientset, d *apps.Deployment) error {
	rsList, err := getReplicaSetsForDeployment(client, d)
	if err != nil {
		return err
	}
	fmt.Println("rsList:")
	for _, rs := range rsList {
		fmt.Println(rs.Name)
	}

	_, allOldRSs := deploymentutil.FindOldReplicaSets(d, rsList)
	fmt.Println("allOldRSs:")
	for _, rs := range allOldRSs {
		fmt.Println(rs.Name)
	}

	_, err = getNewReplicaSet(context.TODO(), client, d, rsList, allOldRSs, true)
	return err
}

func getNewReplicaSet(ctx context.Context, client *kubernetes.Clientset, d *apps.Deployment, rsList, oldRSs []*apps.ReplicaSet, createIfNotExisted bool) (*apps.ReplicaSet, error) {
	existingNewRS := deploymentutil.FindNewReplicaSet(d, rsList)

	// Calculate the max revision number among all old RSes
	maxOldRevision := deploymentutil.MaxRevision(oldRSs)
	// Calculate revision number for this new replica set
	newRevision := strconv.FormatInt(maxOldRevision+1, 10)

	// Latest replica set exists. We need to sync its annotations (includes copying all but
	// annotationsToSkip from the parent deployment, and update revision, desiredReplicas,
	// and maxReplicas) and also update the revision annotation in the deployment with the
	// latest revision.
	if existingNewRS != nil {
		rsCopy := existingNewRS.DeepCopy()

		// Set existing new replica set's annotation
		annotationsUpdated := deploymentutil.SetNewReplicaSetAnnotations(d, rsCopy, &strategy, newRevision, true, maxRevHistoryLengthInChars)
		minReadySecondsNeedsUpdate := rsCopy.Spec.MinReadySeconds != d.Spec.MinReadySeconds
		*rsCopy.Spec.Replicas += 1
		if annotationsUpdated || minReadySecondsNeedsUpdate {
			rsCopy.Spec.MinReadySeconds = d.Spec.MinReadySeconds
			return client.AppsV1().ReplicaSets(rsCopy.ObjectMeta.Namespace).Update(ctx, rsCopy, metav1.UpdateOptions{})
		}

		// Should use the revision in existingNewRS's annotation, since it set by before
		needsUpdate := deploymentutil.SetDeploymentRevision(d, rsCopy.Annotations[deploymentutil.RevisionAnnotation])
		// If no other Progressing condition has been recorded and we need to estimate the progress
		// of this deployment then it is likely that old users started caring about progress. In that
		// case we need to take into account the first time we noticed their new replica set.
		cond := deploymentutil.GetDeploymentCondition(d.Status, apps.DeploymentProgressing)
		if deploymentutil.HasProgressDeadline(d) && cond == nil {
			msg := fmt.Sprintf("Found new replica set %q", rsCopy.Name)
			condition := deploymentutil.NewDeploymentCondition(apps.DeploymentProgressing, v1.ConditionTrue, deploymentutil.FoundNewRSReason, msg)
			deploymentutil.SetDeploymentCondition(&d.Status, *condition)
			needsUpdate = true
		}

		if needsUpdate {
			var err error
			if _, err = client.AppsV1().Deployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
		}
		return rsCopy, nil
	}

	if !createIfNotExisted {
		return nil, nil
	}

	// new ReplicaSet does not exist, create one.
	newRSTemplate := *d.Spec.Template.DeepCopy()
	podTemplateSpecHash := util.ComputeHash(&newRSTemplate, d.Status.CollisionCount)
	newRSTemplate.Labels = labelsutil.CloneAndAddLabel(d.Spec.Template.Labels, apps.DefaultDeploymentUniqueLabelKey, podTemplateSpecHash)
	// Add podTemplateHash label to selector.
	newRSSelector := labelsutil.CloneSelectorAndAddLabel(d.Spec.Selector, apps.DefaultDeploymentUniqueLabelKey, podTemplateSpecHash)

	// Create new ReplicaSet
	newRS := apps.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			// Make the name deterministic, to ensure idempotence
			Name:            d.Name + "-" + podTemplateSpecHash,
			Namespace:       d.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, controllerKind)},
			Labels:          newRSTemplate.Labels,
		},
		Spec: apps.ReplicaSetSpec{
			Replicas:        new(int32),
			MinReadySeconds: d.Spec.MinReadySeconds,
			Selector:        newRSSelector,
			Template:        newRSTemplate,
		},
	}
	allRSs := append(oldRSs, &newRS)
	newReplicasCount, err := deploymentutil.NewRSNewReplicas(d, allRSs, &newRS, &strategy)
	if err != nil {
		return nil, err
	}

	// We ensure that newReplicasLowerBound is greater than 0 unless deployment is 0,
	// this is because if we set new replicas as 0, the native deployment controller
	// will flight with ours.
	newReplicasLowerBound := deploymentutil.NewRSReplicasLowerBound(d, &strategy)

	*(newRS.Spec.Replicas) = integer.Int32Max(newReplicasCount, newReplicasLowerBound)
	// Set new replica set's annotation
	deploymentutil.SetNewReplicaSetAnnotations(d, &newRS, &strategy, newRevision, false, maxRevHistoryLengthInChars)
	// Create the new ReplicaSet. If it already exists, then we need to check for possible
	// hash collisions. If there is any other error, we need to report it in the status of
	// the Deployment.
	alreadyExists := false
	createdRS, err := client.AppsV1().ReplicaSets(d.Namespace).Create(ctx, &newRS, metav1.CreateOptions{})
	switch {
	// We may end up hitting this due to a slow cache or a fast resync of the Deployment.
	case errors.IsAlreadyExists(err):
		alreadyExists = true

		// Fetch a copy of the ReplicaSet.
		// rs, rsErr := dc.rsLister.ReplicaSets(newRS.Namespace).Get(newRS.Name)
		rs, rsErr := client.AppsV1().ReplicaSets(newRS.Namespace).Get(context.TODO(), newRS.Name, metav1.GetOptions{})
		if rsErr != nil {
			return nil, rsErr
		}

		// If the Deployment owns the ReplicaSet and the ReplicaSet's PodTemplateSpec is semantically
		// deep equal to the PodTemplateSpec of the Deployment, it's the Deployment's new ReplicaSet.
		// Otherwise, this is a hash collision and we need to increment the collisionCount field in
		// the status of the Deployment and requeue to try the creation in the next sync.
		controllerRef := metav1.GetControllerOf(rs)
		if controllerRef != nil && controllerRef.UID == d.UID && deploymentutil.EqualIgnoreHash(&d.Spec.Template, &rs.Spec.Template) {
			createdRS = rs
			err = nil
			break
		}

		// Matching ReplicaSet is not equal - increment the collisionCount in the DeploymentStatus
		// and requeue the Deployment.
		if d.Status.CollisionCount == nil {
			d.Status.CollisionCount = new(int32)
		}
		preCollisionCount := *d.Status.CollisionCount
		*d.Status.CollisionCount++
		// Update the collisionCount for the Deployment and let it requeue by returning the original
		// error.
		_, dErr := client.AppsV1().Deployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{})
		if dErr == nil {
			klog.V(2).Infof("Found a hash collision for deployment %q - bumping collisionCount (%d->%d) to resolve it", d.Name, preCollisionCount, *d.Status.CollisionCount)
		}
		return nil, err
	case errors.HasStatusCause(err, v1.NamespaceTerminatingCause):
		// if the namespace is terminating, all subsequent creates will fail and we can safely do nothing
		return nil, err
	case err != nil:
		msg := fmt.Sprintf("Failed to create new replica set %q: %v", newRS.Name, err)
		if deploymentutil.HasProgressDeadline(d) {
			cond := deploymentutil.NewDeploymentCondition(apps.DeploymentProgressing, v1.ConditionFalse, deploymentutil.FailedRSCreateReason, msg)
			deploymentutil.SetDeploymentCondition(&d.Status, *cond)
			// We don't really care about this error at this point, since we have a bigger issue to report.
			// TODO: Identify which errors are permanent and switch DeploymentIsFailed to take into account
			// these reasons as well. Related issue: https://github.com/kubernetes/kubernetes/issues/18568
			_, _ = client.AppsV1().Deployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{})
		}
		fmt.Println(v1.EventTypeWarning, deploymentutil.FailedRSCreateReason, msg)
		return nil, err
	}
	if !alreadyExists && newReplicasCount > 0 {
		fmt.Println(v1.EventTypeNormal, "ScalingReplicaSet", "Scaled up replica set %s to %d", createdRS.Name, newReplicasCount)
	}

	// 重新获取最新的deployment
	d, err = client.AppsV1().Deployments(*ns).Get(context.TODO(), *workload, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	needsUpdate := deploymentutil.SetDeploymentRevision(d, newRevision)
	if !alreadyExists && deploymentutil.HasProgressDeadline(d) {
		msg := fmt.Sprintf("Created new replica set %q", createdRS.Name)
		condition := deploymentutil.NewDeploymentCondition(apps.DeploymentProgressing, v1.ConditionTrue, deploymentutil.NewReplicaSetReason, msg)
		deploymentutil.SetDeploymentCondition(&d.Status, *condition)
		needsUpdate = true
	}
	if needsUpdate {
		_, err = client.AppsV1().Deployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{})
		fmt.Println(d)
		fmt.Println("error:", err)
	}

	return createdRS, err
}

func getReplicaSetsForDeployment(client *kubernetes.Clientset, d *apps.Deployment) ([]*apps.ReplicaSet, error) {
	deploymentSelector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("deployment %s/%s has invalid label selector: %v", d.Namespace, d.Name, err)
	}
	// List all ReplicaSets to find those we own but that no longer match our
	// selector. They will be orphaned by ClaimReplicaSets().
	allRSs, err := client.AppsV1().ReplicaSets(d.Namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: deploymentSelector.String()})
	if err != nil {
		return nil, fmt.Errorf("list %s/%s rs failed:%v", d.Namespace, d.Name, err)
	}
	// select rs owner by current deployment
	ownedRSs := make([]*apps.ReplicaSet, 0)
	for _, rs := range allRSs.Items {
		if !rs.DeletionTimestamp.IsZero() {
			continue
		}

		if metav1.IsControlledBy(&rs, d) {
			ownedRSs = append(ownedRSs, &rs)
		}
	}
	return ownedRSs, nil
}
