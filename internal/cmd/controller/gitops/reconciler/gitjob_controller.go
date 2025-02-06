package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/reugn/go-quartz/quartz"

	fleetutil "github.com/rancher/fleet/internal/cmd/controller/errorutil"
	"github.com/rancher/fleet/internal/cmd/controller/finalize"
	"github.com/rancher/fleet/internal/cmd/controller/imagescan"
	"github.com/rancher/fleet/internal/metrics"
	"github.com/rancher/fleet/internal/names"
	"github.com/rancher/fleet/internal/ociwrapper"
	v1alpha1 "github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	fleetevent "github.com/rancher/fleet/pkg/event"
	"github.com/rancher/fleet/pkg/sharding"

	"github.com/rancher/wrangler/v3/pkg/condition"
	"github.com/rancher/wrangler/v3/pkg/genericcondition"
	"github.com/rancher/wrangler/v3/pkg/kstatus"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	errutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	bundleCAVolumeName        = "additional-ca"
	bundleCAFile              = "additional-ca.crt"
	gitCredentialVolumeName   = "git-credential" // #nosec G101 this is not a credential
	ociRegistryAuthVolumeName = "oci-auth"
	gitClonerVolumeName       = "git-cloner"
	emptyDirVolumeName        = "git-cloner-empty-dir"
	fleetHomeDir              = "/fleet-home"

	defaultPollingSyncInterval = 15 * time.Second
	gitPollingCondition        = "GitPolling"
	generationLabel            = "fleet.cattle.io/gitrepo-generation"
	forceSyncGenerationLabel   = "fleet.cattle.io/force-sync-generation"
)

var zero = int32(0)

type GitFetcher interface {
	LatestCommit(ctx context.Context, gitrepo *v1alpha1.GitRepo, client client.Client) (string, error)
}

// TimeGetter interface is used to mock the time.Now() call in unit tests
type TimeGetter interface {
	Now() time.Time
	Since(t time.Time) time.Duration
}

type RealClock struct{}

func (RealClock) Now() time.Time                  { return time.Now() }
func (RealClock) Since(t time.Time) time.Duration { return time.Since(t) }

// GitJobReconciler reconciles a GitRepo resource to create a git cloning k8s job
type GitJobReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Image           string
	Scheduler       quartz.Scheduler
	Workers         int
	ShardID         string
	JobNodeSelector string
	GitFetcher      GitFetcher
	Clock           TimeGetter
	Recorder        record.EventRecorder
}

func (r *GitJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.GitRepo{},
			builder.WithPredicates(
				// do not trigger for GitRepo status changes (except for commit changes and cache sync)
				predicate.Or(
					TypedResourceVersionUnchangedPredicate[client.Object]{},
					predicate.GenerationChangedPredicate{},
					predicate.AnnotationChangedPredicate{},
					predicate.LabelChangedPredicate{},
					webhookCommitChangedPredicate(),
				),
			),
		).
		Owns(&batchv1.Job{}, builder.WithPredicates(jobUpdatedPredicate())).
		WithEventFilter(sharding.FilterByShardID(r.ShardID)).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Workers}).
		Complete(r)
}

// Reconcile  compares the state specified by the GitRepo object against the
// actual cluster state. It checks the Git repository for new commits and
// creates a job to clone the repository if a new commit is found. In case of
// an error, the output of the job is stored in the status.
func (r *GitJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("gitjob")
	gitrepo := &v1alpha1.GitRepo{}

	if err := r.Get(ctx, req.NamespacedName, gitrepo); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	} else if errors.IsNotFound(err) {
		logger.V(1).Info("Gitrepo deleted, cleaning up poll jobs")
		return ctrl.Result{}, nil
	}

	// Restrictions / Overrides, gitrepo reconciler is responsible for setting error in status
	oldStatus := gitrepo.Status.DeepCopy()
	if err := AuthorizeAndAssignDefaults(ctx, r.Client, gitrepo); err != nil {
		r.Recorder.Event(gitrepo, fleetevent.Warning, "FailedToApplyRestrictions", err.Error())
		return ctrl.Result{}, updateErrorStatus(ctx, r.Client, req.NamespacedName, *oldStatus, err)
	}

	if !gitrepo.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(gitrepo, finalize.GitRepoFinalizer) {
			if err := r.cleanupGitRepo(ctx, logger, gitrepo); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(gitrepo, finalize.GitRepoFinalizer) {
		err := r.addGitRepoFinalizer(ctx, req.NamespacedName)
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}

		// requeue as adding the finalizer changes the spec
		return ctrl.Result{Requeue: true}, nil
	}

	logger = logger.WithValues("generation", gitrepo.Generation, "commit", gitrepo.Status.Commit).WithValues("conditions", gitrepo.Status.Conditions)
	ctx = log.IntoContext(ctx, logger)

	logger.V(1).Info("Reconciling GitRepo")

	if gitrepo.Spec.Repo == "" {
		return ctrl.Result{}, nil
	}

	oldCommit := gitrepo.Status.Commit
	repoPolled, err := r.repoPolled(ctx, gitrepo)
	if err != nil {
		r.Recorder.Event(gitrepo, fleetevent.Warning, "FailedToCheckCommit", err.Error())
		logger.Info("Failed to check for latest commit", "error", err)
	} else if repoPolled && oldCommit != gitrepo.Status.Commit {
		r.Recorder.Event(gitrepo, fleetevent.Normal, "GotNewCommit", gitrepo.Status.Commit)
		logger.Info("New commit from repository", "newCommit", gitrepo.Status.Commit)
	}

	// check for webhook commit
	if gitrepo.Status.WebhookCommit != "" && gitrepo.Status.WebhookCommit != gitrepo.Status.Commit {
		gitrepo.Status.Commit = gitrepo.Status.WebhookCommit
	}

	// From this point onwards we have to take into account if the poller
	// task was executed.
	// If so, we need to return a Result with EnqueueAfter set.

	res, err := r.manageGitJob(ctx, logger, gitrepo, oldCommit, repoPolled)
	if err != nil || res.Requeue {
		return res, err
	}

	setAcceptedCondition(&gitrepo.Status, nil)

	err = updateStatus(ctx, r.Client, req.NamespacedName, gitrepo.Status)
	if err != nil {
		logger.Error(err, "Reconcile failed final update to git repo status", "status", gitrepo.Status)

		return r.result(gitrepo), err
	}

	return r.result(gitrepo), nil
}

// addJitter to the requeue time to avoid thundering herd
// generate a random number between -10% and +10% of the duration
func addJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}

	return d + time.Duration(rand.Int64N(int64(d)/10)) // nolint:gosec // gosec G404 false positive, not used for crypto
}

// manageGitJob is responsible for creating, updating and deleting the GitJob and setting the GitRepo's status accordingly
func (r *GitJobReconciler) manageGitJob(ctx context.Context, logger logr.Logger, gitrepo *v1alpha1.GitRepo, oldCommit string, repoPolled bool) (reconcile.Result, error) {
	name := types.NamespacedName{Namespace: gitrepo.Namespace, Name: gitrepo.Name}
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{
		Namespace: gitrepo.Namespace,
		Name:      jobName(gitrepo),
	}, &job)
	if err != nil && !errors.IsNotFound(err) {
		err = fmt.Errorf("error retrieving git job: %w", err)
		r.Recorder.Event(gitrepo, fleetevent.Warning, "FailedToGetGitJob", err.Error())
		return r.result(gitrepo), err
	}

	if errors.IsNotFound(err) {
		if gitrepo.Spec.DisablePolling {
			commit, err := r.GitFetcher.LatestCommit(ctx, gitrepo, r.Client)
			condition.Cond(gitPollingCondition).SetError(&gitrepo.Status, "", err)
			if err == nil && commit != "" {
				gitrepo.Status.Commit = commit
			}
			if err != nil {
				r.Recorder.Event(gitrepo, fleetevent.Warning, "Failed", err.Error())
			} else {
				if repoPolled && oldCommit != gitrepo.Status.Commit {
					r.Recorder.Event(gitrepo, fleetevent.Normal, "GotNewCommit", gitrepo.Status.Commit)
				}
			}
		}

		if r.shouldCreateJob(gitrepo, oldCommit) {
			r.updateGenerationValuesIfNeeded(gitrepo)
			if err := r.validateExternalSecretExist(ctx, gitrepo); err != nil {
				r.Recorder.Event(gitrepo, fleetevent.Warning, "FailedValidatingSecret", err.Error())
				return r.result(gitrepo), updateErrorStatus(ctx, r.Client, name, gitrepo.Status, err)
			}
			if err := r.createJobAndResources(ctx, gitrepo, logger); err != nil {
				return r.result(gitrepo), err
			}
		}
	} else if gitrepo.Status.Commit != "" && gitrepo.Status.Commit == oldCommit {
		err, recreateGitJob := r.deleteJobIfNeeded(ctx, gitrepo, &job)
		if err != nil {
			return r.result(gitrepo), fmt.Errorf("error deleting git job: %w", err)
		}
		// job was deleted and we need to recreate it
		// Requeue so the reconciler creates the job again
		if recreateGitJob {
			return reconcile.Result{Requeue: true}, nil
		}
	}

	gitrepo.Status.ObservedGeneration = gitrepo.Generation

	if err = setStatusFromGitjob(ctx, r.Client, gitrepo, &job); err != nil {
		return r.result(gitrepo), updateErrorStatus(ctx, r.Client, name, gitrepo.Status, err)
	}

	return reconcile.Result{}, nil
}

func (r *GitJobReconciler) cleanupGitRepo(ctx context.Context, logger logr.Logger, gitrepo *v1alpha1.GitRepo) error {
	logger.Info("Gitrepo deleted, deleting bundle, image scans")

	metrics.GitRepoCollector.Delete(gitrepo.Name, gitrepo.Namespace)

	nsName := types.NamespacedName{Name: gitrepo.Name, Namespace: gitrepo.Namespace}
	if err := finalize.PurgeBundles(ctx, r.Client, nsName, v1alpha1.RepoLabel); err != nil {
		return err
	}

	// remove the job scheduled by imagescan, if any
	_ = r.Scheduler.DeleteJob(imagescan.GitCommitKey(gitrepo.Namespace, gitrepo.Name))

	if err := finalize.PurgeImageScans(ctx, r.Client, nsName); err != nil {
		return err
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, nsName, gitrepo); err != nil {
			return err
		}

		controllerutil.RemoveFinalizer(gitrepo, finalize.GitRepoFinalizer)

		return r.Update(ctx, gitrepo)
	})

	if client.IgnoreNotFound(err) != nil {
		return err
	}

	return nil
}

// shouldCreateJob checks if the conditions to create a new job are met.
// It checks for all the conditions so, in case more than one is met, it sets all the
// values related in one single reconciler loop
func (r *GitJobReconciler) shouldCreateJob(gitrepo *v1alpha1.GitRepo, oldCommit string) bool {
	if gitrepo.Status.Commit != "" && gitrepo.Status.Commit != oldCommit {
		return true
	}

	if gitrepo.Spec.ForceSyncGeneration != gitrepo.Status.UpdateGeneration {
		return true
	}

	// k8s Jobs are immutable. Recreate the job if the GitRepo Spec has changed.
	// Avoid deleting the job twice
	if generationChanged(gitrepo) {
		return true
	}

	return false
}

func (r *GitJobReconciler) updateGenerationValuesIfNeeded(gitrepo *v1alpha1.GitRepo) {
	if gitrepo.Spec.ForceSyncGeneration != gitrepo.Status.UpdateGeneration {
		gitrepo.Status.UpdateGeneration = gitrepo.Spec.ForceSyncGeneration
	}

	if generationChanged(gitrepo) {
		gitrepo.Status.ObservedGeneration = gitrepo.Generation
	}
}

func (r *GitJobReconciler) addGitRepoFinalizer(ctx context.Context, nsName types.NamespacedName) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		gitrepo := &v1alpha1.GitRepo{}
		if err := r.Get(ctx, nsName, gitrepo); err != nil {
			return err
		}

		controllerutil.AddFinalizer(gitrepo, finalize.GitRepoFinalizer)

		return r.Update(ctx, gitrepo)
	})

	if err != nil {
		return client.IgnoreNotFound(err)
	}

	return nil
}

func (r *GitJobReconciler) createJobRBAC(ctx context.Context, gitrepo *v1alpha1.GitRepo) error {
	// No update needed, values are the same. So we ignore AlreadyExists.
	saName := names.SafeConcatName("git", gitrepo.Name)
	sa := newServiceAccount(gitrepo.Namespace, saName)
	if err := controllerutil.SetControllerReference(gitrepo, sa, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, sa); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}

	role := newRole(gitrepo.Namespace, saName)
	if err := controllerutil.SetControllerReference(gitrepo, role, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, role); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}

	rb := newRoleBinding(gitrepo.Namespace, saName)
	if err := controllerutil.SetControllerReference(gitrepo, rb, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, rb); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}

	return nil
}

func (r *GitJobReconciler) createTargetsConfigMap(ctx context.Context, gitrepo *v1alpha1.GitRepo) error {
	configMap, err := newTargetsConfigMap(gitrepo)
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(gitrepo, configMap, r.Scheme); err != nil {
		return err
	}
	data := configMap.BinaryData
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		configMap.BinaryData = data
		return nil
	})

	return err
}

func (r *GitJobReconciler) createCABundleSecret(ctx context.Context, gitrepo *v1alpha1.GitRepo) error {
	if len(gitrepo.Spec.CABundle) == 0 {
		return nil
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: gitrepo.ObjectMeta.Namespace,
			Name:      caBundleName(gitrepo),
		},
		Data: map[string][]byte{
			bundleCAFile: gitrepo.Spec.CABundle,
		},
	}
	if err := controllerutil.SetControllerReference(gitrepo, secret, r.Scheme); err != nil {
		return err
	}
	data := secret.StringData
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.StringData = data
		return nil
	})

	return err
}

func (r *GitJobReconciler) validateExternalSecretExist(ctx context.Context, gitrepo *v1alpha1.GitRepo) error {
	if gitrepo.Spec.HelmSecretNameForPaths != "" {
		if err := r.Get(ctx, types.NamespacedName{Namespace: gitrepo.Namespace, Name: gitrepo.Spec.HelmSecretNameForPaths}, &corev1.Secret{}); err != nil {
			return fmt.Errorf("failed to look up HelmSecretNameForPaths, error: %w", err)
		}
	} else if gitrepo.Spec.HelmSecretName != "" {
		if err := r.Get(ctx, types.NamespacedName{Namespace: gitrepo.Namespace, Name: gitrepo.Spec.HelmSecretName}, &corev1.Secret{}); err != nil {
			return fmt.Errorf("failed to look up helmSecretName, error: %w", err)
		}
	}
	return nil
}

func (r *GitJobReconciler) createJob(ctx context.Context, gitRepo *v1alpha1.GitRepo) error {
	job, err := r.newGitJob(ctx, gitRepo)
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(gitRepo, job, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, job)
}

func (r *GitJobReconciler) createJobAndResources(ctx context.Context, gitrepo *v1alpha1.GitRepo, logger logr.Logger) error {
	logger.V(1).Info("Creating Git job resources")
	if err := r.createJobRBAC(ctx, gitrepo); err != nil {
		return fmt.Errorf("failed to create RBAC resources for git job: %w", err)
	}
	if err := r.createTargetsConfigMap(ctx, gitrepo); err != nil {
		return fmt.Errorf("failed to create targets config map for git job: %w", err)
	}
	if err := r.createCABundleSecret(ctx, gitrepo); err != nil {
		return fmt.Errorf("failed to create cabundle secret for git job: %w", err)
	}
	if err := r.createJob(ctx, gitrepo); err != nil {
		return fmt.Errorf("error creating git job: %w", err)
	}
	r.Recorder.Event(gitrepo, fleetevent.Normal, "Created", "GitJob was created")
	return nil
}

func (r *GitJobReconciler) deleteJobIfNeeded(ctx context.Context, gitRepo *v1alpha1.GitRepo, job *batchv1.Job) (error, bool) {
	logger := log.FromContext(ctx)

	// the following cases imply that the job is still running but we need to stop it and
	// create a new one
	if gitRepo.Spec.ForceSyncGeneration != gitRepo.Status.UpdateGeneration {
		if forceSync, ok := job.Labels[forceSyncGenerationLabel]; ok {
			t := fmt.Sprintf("%d", gitRepo.Spec.ForceSyncGeneration)
			if t != forceSync {
				jobDeletedMessage := "job deletion triggered because of ForceUpdateGeneration"
				logger.V(1).Info(jobDeletedMessage)
				if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) {
					return err, true
				}
				return nil, true
			}
		}
	}

	// k8s Jobs are immutable. Recreate the job if the GitRepo Spec has changed.
	// Avoid deleting the job twice
	if generationChanged(gitRepo) {
		if gen, ok := job.Labels[generationLabel]; ok {
			t := fmt.Sprintf("%d", gitRepo.Generation)
			if t != gen {
				jobDeletedMessage := "job deletion triggered because of generation change"
				logger.V(1).Info(jobDeletedMessage)
				if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) {
					return err, true
				}
				return nil, true
			}
		}
	}

	// check if the job finished and was successful
	if job.Status.Succeeded == 1 {
		jobDeletedMessage := "job deletion triggered because job succeeded"
		logger.Info(jobDeletedMessage)
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) {
			return err, false
		}
		r.Recorder.Event(gitRepo, fleetevent.Normal, "JobDeleted", jobDeletedMessage)
	}

	return nil, false
}

func generationChanged(r *v1alpha1.GitRepo) bool {
	// checks if generation changed.
	// it ignores the case when Status.ObservedGeneration=0 because that's
	// the initial value of a just created GitRepo and the initial value
	// for Generation in k8s is 1.
	// If we don't ignore we would be deleting the gitjob that was just created
	// until later we reconcile ObservedGeneration with Generation
	return (r.Generation != r.Status.ObservedGeneration) && r.Status.ObservedGeneration > 0
}

func jobName(obj *v1alpha1.GitRepo) string {
	return names.SafeConcatName(obj.Name, names.Hex(obj.Spec.Repo+obj.Status.Commit, 5))
}

func caBundleName(obj *v1alpha1.GitRepo) string {
	return fmt.Sprintf("%s-cabundle", obj.Name)
}

func (r *GitJobReconciler) newGitJob(ctx context.Context, obj *v1alpha1.GitRepo) (*batchv1.Job, error) {
	jobSpec, err := r.newJobSpec(ctx, obj)
	if err != nil {
		return nil, err
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"generation": strconv.Itoa(int(obj.Generation)),
				"commit":     obj.Status.Commit,
			},
			Labels: map[string]string{
				forceSyncGenerationLabel: fmt.Sprintf("%d", obj.Spec.ForceSyncGeneration),
				generationLabel:          fmt.Sprintf("%d", obj.Generation),
			},
			Namespace: obj.Namespace,
			Name:      jobName(obj),
		},
		Spec: *jobSpec,
	}
	// if the repo references a shard, add the same label to the job
	// this avoids a call to Reconcile for controllers that do not match
	// the shard-id
	label, hasLabel := obj.GetLabels()[sharding.ShardingRefLabel]
	if hasLabel {
		job.Labels = labels.Merge(job.Labels, map[string]string{
			sharding.ShardingRefLabel: label,
		})
	}

	initContainer, err := r.newGitCloner(ctx, obj)
	if err != nil {
		return nil, err
	}
	job.Spec.Template.Spec.InitContainers = []corev1.Container{initContainer}
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes,
		corev1.Volume{
			Name: gitClonerVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}, corev1.Volume{
			Name: emptyDirVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	)

	if obj.Spec.CABundle != nil {
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: bundleCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: caBundleName(obj),
				},
			},
		})
	}

	if obj.Spec.ClientSecretName != "" {
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: gitCredentialVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: obj.Spec.ClientSecretName,
					},
				},
			},
		)
	}

	for i := range job.Spec.Template.Spec.Containers {
		job.Spec.Template.Spec.Containers[i].VolumeMounts = append(job.Spec.Template.Spec.Containers[i].VolumeMounts, corev1.VolumeMount{
			MountPath: "/workspace/source",
			Name:      gitClonerVolumeName,
		})
		job.Spec.Template.Spec.Containers[i].Env = append(job.Spec.Template.Spec.Containers[i].Env,
			corev1.EnvVar{
				Name:  "COMMIT",
				Value: obj.Status.Commit,
			},
		)
		job.Spec.Template.Spec.Containers[i].Env = append(job.Spec.Template.Spec.Containers[i].Env, proxyEnvVars()...)
	}

	return job, nil
}

func (r *GitJobReconciler) newJobSpec(ctx context.Context, gitrepo *v1alpha1.GitRepo) (*batchv1.JobSpec, error) {
	paths := gitrepo.Spec.Paths
	if len(paths) == 0 {
		paths = []string{"."}
	}

	// compute configmap, needed because its name contains a hash
	configMap, err := newTargetsConfigMap(gitrepo)
	if err != nil {
		return nil, err
	}

	volumes, volumeMounts := volumes(configMap.Name)

	if gitrepo.Spec.HelmSecretNameForPaths != "" {
		vols, volMnts := volumesFromSecret(ctx, r.Client,
			gitrepo.Namespace,
			gitrepo.Spec.HelmSecretNameForPaths,
			"helm-secret-by-path",
		)

		volumes = append(volumes, vols...)
		volumeMounts = append(volumeMounts, volMnts...)

	} else if gitrepo.Spec.HelmSecretName != "" {
		vols, volMnts := volumesFromSecret(ctx, r.Client,
			gitrepo.Namespace,
			gitrepo.Spec.HelmSecretName,
			"helm-secret",
		)

		volumes = append(volumes, vols...)
		volumeMounts = append(volumeMounts, volMnts...)
	}

	if ociwrapper.ExperimentalOCIIsEnabled() && gitrepo.Spec.OCIRegistry != nil && gitrepo.Spec.OCIRegistry.AuthSecretName != "" {
		vol, volMnt, err := ociVolumeFromSecret(ctx, r.Client,
			gitrepo.Namespace,
			gitrepo.Spec.OCIRegistry.AuthSecretName,
			ociRegistryAuthVolumeName,
		)
		if err != nil {
			return nil, err
		}

		volumes = append(volumes, vol)
		volumeMounts = append(volumeMounts, volMnt)
	}

	shardID := gitrepo.Labels[sharding.ShardingRefLabel]

	nodeSelector := map[string]string{"kubernetes.io/os": "linux"}
	if shardID != "" && len(strings.TrimSpace(r.JobNodeSelector)) > 0 {
		var shardNodeSelector map[string]string
		if err := json.Unmarshal([]byte(r.JobNodeSelector), &shardNodeSelector); err != nil {
			return nil, fmt.Errorf("could not decode shard node selector: %w", err)
		}

		for k, v := range shardNodeSelector {
			nodeSelector[k] = v
		}
	}

	saName := names.SafeConcatName("git", gitrepo.Name)
	logger := log.FromContext(ctx)
	args, envs := argsAndEnvs(gitrepo, logger.V(1).Enabled())

	return &batchv1.JobSpec{
		BackoffLimit: &zero,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.Time{Time: time.Unix(0, 0)},
			},
			Spec: corev1.PodSpec{
				Volumes: volumes,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsUser: &[]int64{1000}[0],
				},
				ServiceAccountName: saName,
				RestartPolicy:      corev1.RestartPolicyNever,
				Containers: []corev1.Container{
					{
						Name:         "fleet",
						Image:        r.Image,
						Command:      []string{"log.sh"},
						Args:         append(args, paths...),
						WorkingDir:   "/workspace/source",
						VolumeMounts: volumeMounts,
						Env:          envs,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &[]bool{false}[0],
							ReadOnlyRootFilesystem:   &[]bool{true}[0],
							Privileged:               &[]bool{false}[0],
							RunAsNonRoot:             &[]bool{true}[0],
							SeccompProfile: &corev1.SeccompProfile{
								Type: corev1.SeccompProfileTypeRuntimeDefault,
							},
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					},
				},
				NodeSelector: nodeSelector,
				Tolerations: []corev1.Toleration{
					{
						Key:      "cattle.io/os",
						Operator: "Equal",
						Value:    "linux",
						Effect:   "NoSchedule",
					},
					{
						Key:      "node.cloudprovider.kubernetes.io/uninitialized",
						Operator: "Equal",
						Value:    "true",
						Effect:   "NoSchedule",
					},
				},
			},
		},
	}, nil
}

func argsAndEnvs(gitrepo *v1alpha1.GitRepo, debug bool) ([]string, []corev1.EnvVar) {
	args := []string{
		"fleet",
		"apply",
	}

	if debug {
		args = append(args, "--debug", "--debug-level", "9")
	}

	bundleLabels := labels.Merge(gitrepo.Labels, map[string]string{
		v1alpha1.RepoLabel: gitrepo.Name,
	})

	args = append(args,
		"--targets-file=/run/config/targets.yaml",
		"--label="+bundleLabels.String(),
		"--namespace", gitrepo.Namespace,
		"--service-account", gitrepo.Spec.ServiceAccount,
		fmt.Sprintf("--sync-generation=%d", gitrepo.Spec.ForceSyncGeneration),
		fmt.Sprintf("--paused=%v", gitrepo.Spec.Paused),
		"--target-namespace", gitrepo.Spec.TargetNamespace,
	)

	if gitrepo.Spec.KeepResources {
		args = append(args, "--keep-resources")
	}

	if gitrepo.Spec.DeleteNamespace {
		args = append(args, "--delete-namespace")
	}

	if gitrepo.Spec.CorrectDrift != nil && gitrepo.Spec.CorrectDrift.Enabled {
		args = append(args, "--correct-drift")
		if gitrepo.Spec.CorrectDrift.Force {
			args = append(args, "--correct-drift-force")
		}
		if gitrepo.Spec.CorrectDrift.KeepFailHistory {
			args = append(args, "--correct-drift-keep-fail-history")
		}
	}

	env := []corev1.EnvVar{
		{
			Name:  "HOME",
			Value: fleetHomeDir,
		},
	}
	if gitrepo.Spec.HelmSecretNameForPaths != "" {
		helmArgs := []string{
			"--helm-credentials-by-path-file",
			"/etc/fleet/helm/secrets-path.yaml",
		}

		args = append(args, helmArgs...)
		env = append(env,
			// for ssh go-getter, make sure we always accept new host key
			corev1.EnvVar{
				Name:  "GIT_SSH_COMMAND",
				Value: "ssh -o stricthostkeychecking=accept-new",
			},
		)
	} else if gitrepo.Spec.HelmSecretName != "" {
		helmArgs := []string{
			"--password-file",
			"/etc/fleet/helm/password",
			"--cacerts-file",
			"/etc/fleet/helm/cacerts",
			"--ssh-privatekey-file",
			"/etc/fleet/helm/ssh-privatekey",
		}
		if gitrepo.Spec.HelmRepoURLRegex != "" {
			helmArgs = append(helmArgs, "--helm-repo-url-regex", gitrepo.Spec.HelmRepoURLRegex)
		}
		args = append(args, helmArgs...)
		env = append(env,
			// for ssh go-getter, make sure we always accept new host key
			corev1.EnvVar{
				Name:  "GIT_SSH_COMMAND",
				Value: "ssh -o stricthostkeychecking=accept-new",
			},
			corev1.EnvVar{
				Name: "HELM_USERNAME",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						Optional: &[]bool{true}[0],
						Key:      "username",
						LocalObjectReference: corev1.LocalObjectReference{
							Name: gitrepo.Spec.HelmSecretName,
						},
					},
				},
			})
	}

	if ociwrapper.ExperimentalOCIIsEnabled() && gitrepo.Spec.OCIRegistry != nil && gitrepo.Spec.OCIRegistry.Reference != "" {
		args = append(args, "--oci-reference", gitrepo.Spec.OCIRegistry.Reference)
		if gitrepo.Spec.OCIRegistry.AuthSecretName != "" {
			args = append(args, "--oci-password-file", "/etc/fleet/oci/password")
			env = append(env,
				corev1.EnvVar{
					Name: "OCI_USERNAME",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							Optional: &[]bool{true}[0],
							Key:      "username",
							LocalObjectReference: corev1.LocalObjectReference{
								Name: gitrepo.Spec.OCIRegistry.AuthSecretName,
							},
						},
					},
				})
		}
		if gitrepo.Spec.OCIRegistry.BasicHTTP {
			args = append(args, "--oci-basic-http")
		}
		if gitrepo.Spec.OCIRegistry.InsecureSkipTLS {
			args = append(args, "--oci-insecure")
		}
	}

	return append(args, "--", gitrepo.Name), env
}

// volumes builds sets of volumes and their volume mounts for default folders and the targets config map.
func volumes(targetsConfigName string) ([]corev1.Volume, []corev1.VolumeMount) {
	const (
		emptyDirTmpVolumeName  = "fleet-tmp-empty-dir"
		emptyDirHomeVolumeName = "fleet-home-empty-dir"
		configVolumeName       = "config"
	)

	volumes := []corev1.Volume{
		{
			Name: configVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: targetsConfigName,
					},
				},
			},
		},
		{
			Name: emptyDirTmpVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: emptyDirHomeVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      configVolumeName,
			MountPath: "/run/config",
		},
		{
			Name:      emptyDirTmpVolumeName,
			MountPath: "/tmp",
		},
		{
			Name:      emptyDirHomeVolumeName,
			MountPath: fleetHomeDir,
		},
	}

	return volumes, volumeMounts
}

// ociVolumeFromSecret generates a volume and volume mount from a basic-auth secret.
func ociVolumeFromSecret(
	ctx context.Context,
	c client.Client,
	namespace, secretName, volumeName string,
) (corev1.Volume, corev1.VolumeMount, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      secretName,
	}, &secret); err != nil {
		return corev1.Volume{}, corev1.VolumeMount{}, err
	}
	volume := corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
			},
		},
	}
	volumeMount := corev1.VolumeMount{
		Name:      volumeName,
		MountPath: "/etc/fleet/oci",
	}
	return volume, volumeMount, nil
}

// volumesFromSecret generates volumes and volume mounts from a Helm secret, assuming that that secret exists.
// If the secret has a cacerts key, it will be mounted into /etc/ssl/certs, too.
func volumesFromSecret(
	ctx context.Context,
	c client.Client,
	namespace string,
	secretName, volumeName string,
) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := []corev1.Volume{
		{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName,
				},
			},
		},
	}
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      volumeName,
			MountPath: "/etc/fleet/helm",
		},
	}

	// Mount a CA certificate, if specified in the secret. This is necessary to support Helm registries with
	// self-signed certificates.
	secret := &corev1.Secret{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret)
	if _, ok := secret.Data["cacerts"]; ok {
		certVolumeName := fmt.Sprintf("%s-cert", volumeName)

		volumes = append(volumes, corev1.Volume{
			Name: certVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName,
					Items: []corev1.KeyToPath{
						{
							Key:  "cacerts",
							Path: "cacert.crt",
						},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      certVolumeName,
			MountPath: "/etc/ssl/certs",
		})
	}

	return volumes, volumeMounts
}

func (r *GitJobReconciler) newGitCloner(ctx context.Context, obj *v1alpha1.GitRepo) (corev1.Container, error) {
	args := []string{"gitcloner", obj.Spec.Repo, "/workspace"}
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      gitClonerVolumeName,
			MountPath: "/workspace",
		},
		{
			Name:      emptyDirVolumeName,
			MountPath: "/tmp",
		},
	}

	branch, rev := obj.Spec.Branch, obj.Spec.Revision
	if branch != "" {
		args = append(args, "--branch", branch)
	} else if rev != "" {
		args = append(args, "--revision", rev)
	} else {
		args = append(args, "--branch", "master")
	}

	if obj.Spec.ClientSecretName != "" {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: obj.Namespace,
			Name:      obj.Spec.ClientSecretName,
		}, &secret); err != nil {
			return corev1.Container{}, err
		}

		switch secret.Type {
		case corev1.SecretTypeBasicAuth:
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      gitCredentialVolumeName,
				MountPath: "/gitjob/credentials",
			})
			args = append(args, "--username", string(secret.Data[corev1.BasicAuthUsernameKey]))
			args = append(args, "--password-file", "/gitjob/credentials/"+corev1.BasicAuthPasswordKey)
		case corev1.SecretTypeSSHAuth:
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      gitCredentialVolumeName,
				MountPath: "/gitjob/ssh",
			})
			args = append(args, "--ssh-private-key-file", "/gitjob/ssh/"+corev1.SSHAuthPrivateKey)
			knownHosts := secret.Data["known_hosts"]
			if knownHosts != nil {
				args = append(args, "--known-hosts-file", "/gitjob/ssh/known_hosts")
			}
		}
	}

	if obj.Spec.InsecureSkipTLSverify {
		args = append(args, "--insecure-skip-tls")
	}
	if obj.Spec.CABundle != nil {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      bundleCAVolumeName,
			MountPath: "/gitjob/cabundle",
		})
		args = append(args, "--ca-bundle-file", "/gitjob/cabundle/"+bundleCAFile)
	}

	return corev1.Container{
		Command: []string{
			"fleet",
		},
		Args:         args,
		Image:        r.Image,
		Name:         "gitcloner-initializer",
		VolumeMounts: volumeMounts,
		Env:          proxyEnvVars(),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &[]bool{false}[0],
			ReadOnlyRootFilesystem:   &[]bool{true}[0],
			Privileged:               &[]bool{false}[0],
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			RunAsNonRoot:             &[]bool{true}[0],
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}, nil
}

func proxyEnvVars() []corev1.EnvVar {
	var envVars []corev1.EnvVar
	for _, envVar := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if val, ok := os.LookupEnv(envVar); ok {
			envVars = append(envVars, corev1.EnvVar{Name: envVar, Value: val})
		}
	}

	return envVars
}

// repoPolled returns true if the git poller was executed and the repo should still be polled.
func (r *GitJobReconciler) repoPolled(ctx context.Context, gitrepo *v1alpha1.GitRepo) (bool, error) {
	if gitrepo.Spec.DisablePolling {
		return false, nil
	}
	if r.shouldRunPollingTask(gitrepo) {
		gitrepo.Status.LastPollingTime.Time = r.Clock.Now()
		commit, err := r.GitFetcher.LatestCommit(ctx, gitrepo, r.Client)
		condition.Cond(gitPollingCondition).SetError(&gitrepo.Status, "", err)
		if err != nil {
			return true, err
		}
		gitrepo.Status.Commit = commit

		return true, nil
	}

	return false, nil
}

func (r *GitJobReconciler) shouldRunPollingTask(gitrepo *v1alpha1.GitRepo) bool {
	if gitrepo.Spec.DisablePolling {
		return false
	}

	t := gitrepo.Status.LastPollingTime

	if t.IsZero() || (r.Clock.Since(t.Time) >= getPollingIntervalDuration(gitrepo)) {
		return true
	}
	if gitrepo.Status.ObservedGeneration != gitrepo.Generation {
		return true
	}
	return false
}

func getPollingIntervalDuration(gitrepo *v1alpha1.GitRepo) time.Duration {
	if gitrepo.Spec.PollingInterval == nil || gitrepo.Spec.PollingInterval.Duration == 0 {
		return defaultPollingSyncInterval
	}

	return gitrepo.Spec.PollingInterval.Duration
}

func (r *GitJobReconciler) result(gitrepo *v1alpha1.GitRepo) reconcile.Result {
	// We always return a reconcile Result with RequeueAfter set to the polling interval
	// unless polling is disabled.
	// This is done to ensure the polling cycle is never broken due to race conditions
	// between regular events and RequeueAfter events.
	// Requeuing more events when there is already an event in the queue is not a problem
	// because controller-runtime ignores events with higher timestamp
	// For example, if we have an event in the queue that should be executed at time X
	// and we try to enqueue another event that should be executed at time X+10 it will be
	// dropped.
	// If we try to enqueue an event at time X-10, it will replace the one in the queue.
	// The queue will always keep the event that should be triggered earlier.
	if gitrepo.Spec.DisablePolling {
		return reconcile.Result{}
	}

	// Calculate next reconciliation schedule based on the elapsed time since the last polling
	// so it matches the configured polling interval.
	// A fixed value may lead to drifts due to out-of-schedule reconciliations.
	requeueAfter := getPollingIntervalDuration(gitrepo) - r.Clock.Since(gitrepo.Status.LastPollingTime.Time)
	if requeueAfter <= 0 {
		// This is a protection for cases in which the calculation above is 0 or less.
		// In those cases controller-runtime does not call AddAfter for this object and
		// the RequeueAfter cycle is lost.
		// To ensure that this cycle is not broken we force the object to be requeued.
		return reconcile.Result{Requeue: true}
	}
	requeueAfter = addJitter(requeueAfter)
	return reconcile.Result{RequeueAfter: requeueAfter}
}

func webhookCommitChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldGitRepo, ok := e.ObjectOld.(*v1alpha1.GitRepo)
			if !ok {
				return true
			}
			newGitRepo, ok := e.ObjectNew.(*v1alpha1.GitRepo)
			if !ok {
				return true
			}
			return oldGitRepo.Status.WebhookCommit != newGitRepo.Status.WebhookCommit
		},
	}
}

func jobUpdatedPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			n, isJob := e.ObjectNew.(*batchv1.Job)
			if !isJob {
				return false
			}
			o := e.ObjectOld.(*batchv1.Job)
			if n == nil || o == nil {
				return false
			}
			return !reflect.DeepEqual(n.Status, o.Status)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
}

// setStatusFromGitjob sets the status fields relative to the given job in the gitRepo
func setStatusFromGitjob(ctx context.Context, c client.Client, gitRepo *v1alpha1.GitRepo, job *batchv1.Job) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(job)
	if err != nil {
		return err
	}
	uJob := &unstructured.Unstructured{Object: obj}

	result, err := status.Compute(uJob)
	if err != nil {
		return err
	}

	terminationMessage := ""
	if result.Status == status.FailedStatus {
		selector := labels.SelectorFromSet(labels.Set{"job-name": job.Name})
		podList := &corev1.PodList{}
		err := c.List(ctx, podList, &client.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}

		sort.Slice(podList.Items, func(i, j int) bool {
			return podList.Items[i].CreationTimestamp.Before(&podList.Items[j].CreationTimestamp)
		})

		terminationMessage = result.Message
		if len(podList.Items) > 0 {
			for _, podStatus := range podList.Items[len(podList.Items)-1].Status.ContainerStatuses {
				if podStatus.Name != "step-git-source" && podStatus.State.Terminated != nil {
					terminationMessage += podStatus.State.Terminated.Message
				}
			}
		}
	}

	gitRepo.Status.GitJobStatus = result.Status.String()

	for _, con := range result.Conditions {
		if con.Type.String() == "Ready" {
			continue
		}
		condition.Cond(con.Type.String()).SetStatus(gitRepo, string(con.Status))
		condition.Cond(con.Type.String()).SetMessageIfBlank(gitRepo, con.Message)
		condition.Cond(con.Type.String()).Reason(gitRepo, con.Reason)
	}

	// status.Compute() possible results are
	//   - InProgress
	//   - Current
	//   - Failed
	//   - Terminating
	switch result.Status {
	case status.FailedStatus:
		kstatus.SetError(gitRepo, terminationMessage)
	case status.CurrentStatus:
		if strings.Contains(result.Message, "Job Completed") {
			gitRepo.Status.Commit = job.Annotations["commit"]
		}
		kstatus.SetActive(gitRepo)
	case status.InProgressStatus:
		kstatus.SetTransitioning(gitRepo, "")
	case status.TerminatingStatus:
		// set active set both conditions to False
		// the job is terminating so avoid reporting errors in
		// that case
		kstatus.SetActive(gitRepo)
	}

	return nil
}

// setAcceptedCondition sets the condition and updates the timestamp, if the condition changed
func setAcceptedCondition(status *v1alpha1.GitRepoStatus, err error) {
	cond := condition.Cond(v1alpha1.GitRepoAcceptedCondition)
	origStatus := status.DeepCopy()
	cond.SetError(status, "", fleetutil.IgnoreConflict(err))
	if !equality.Semantic.DeepEqual(origStatus, status) {
		cond.LastUpdated(status, time.Now().UTC().Format(time.RFC3339))
	}
}

// updateErrorStatus sets the condition in the status and tries to update the resource
func updateErrorStatus(ctx context.Context, c client.Client, req types.NamespacedName, status v1alpha1.GitRepoStatus, orgErr error) error {
	setAcceptedCondition(&status, orgErr)
	if statusErr := updateStatus(ctx, c, req, status); statusErr != nil {
		merr := []error{orgErr, fmt.Errorf("failed to update the status: %w", statusErr)}
		return errutil.NewAggregate(merr)
	}
	return orgErr
}

// updateStatus updates the status for the GitRepo resource. It retries on
// conflict. If the status was updated successfully, it also collects (as in
// updates) metrics for the resource GitRepo resource.
func updateStatus(ctx context.Context, c client.Client, req types.NamespacedName, status v1alpha1.GitRepoStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		t := &v1alpha1.GitRepo{}
		err := c.Get(ctx, req, t)
		if err != nil {
			return err
		}

		commit := t.Status.Commit

		// selectively update the status fields this reconciler is responsible for
		t.Status.Commit = status.Commit
		t.Status.GitJobStatus = status.GitJobStatus
		t.Status.LastPollingTime = status.LastPollingTime
		t.Status.ObservedGeneration = status.ObservedGeneration
		t.Status.UpdateGeneration = status.UpdateGeneration

		// only keep the Ready condition from live status, it's calculated by the status reconciler
		conds := []genericcondition.GenericCondition{}
		for _, c := range t.Status.Conditions {
			if c.Type == "Ready" {
				conds = append(conds, c)
				break
			}
		}
		for _, c := range status.Conditions {
			if c.Type == "Ready" {
				continue
			}
			conds = append(conds, c)
		}
		t.Status.Conditions = conds

		if commit != "" && status.Commit == "" {
			// we could incur in a race condition between the poller job
			// setting the Commit and the first time the reconciler runs.
			// The poller could be faster than the reconciler setting the
			// Commit and we could reset back to "" in here
			t.Status.Commit = commit
		}

		err = c.Status().Update(ctx, t)
		if err != nil {
			return err
		}

		metrics.GitRepoCollector.Collect(ctx, t)

		return nil
	})
}
