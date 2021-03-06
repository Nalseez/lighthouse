package launcher

import (
	"os"
	"strconv"

	"github.com/jenkins-x/go-scm/scm"
	jxclient "github.com/jenkins-x/jx/pkg/client/clientset/versioned"
	"github.com/jenkins-x/jx/pkg/tekton/metapipeline"
	"github.com/jenkins-x/lighthouse/pkg/apis/lighthouse/v1alpha1"
	clientset "github.com/jenkins-x/lighthouse/pkg/client/clientset/versioned"
	"github.com/jenkins-x/lighthouse/pkg/util"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// launcher default launcher
type launcher struct {
	jxClient  jxclient.Interface
	lhClient  clientset.Interface
	namespace string
}

// NewLauncher creates a new builder
func NewLauncher(jxClient jxclient.Interface, lhClient clientset.Interface, namespace string) (PipelineLauncher, error) {
	b := &launcher{
		jxClient:  jxClient,
		lhClient:  lhClient,
		namespace: namespace,
	}
	return b, nil
}

// Launch creates a pipeline
// TODO: This should be moved somewhere else, probably, and needs some kind of unit testing (apb)
func (b *launcher) Launch(request *v1alpha1.LighthouseJob, metapipelineClient metapipeline.Client, repository scm.Repository) (*v1alpha1.LighthouseJob, error) {
	spec := &request.Spec

	name := repository.Name
	owner := repository.Namespace
	sourceURL := repository.Clone

	pullRefData := b.getPullRefs(sourceURL, spec)
	pullRefs := ""
	if len(spec.Refs.Pulls) > 0 {
		pullRefs = pullRefData.String()
	}

	branch := spec.GetBranch()
	if branch == "" {
		branch = repository.Branch
	}
	if branch == "" {
		branch = "master"
	}
	if pullRefs == "" {
		pullRefs = branch + ":"
	}

	job := spec.Job
	var kind metapipeline.PipelineKind
	if len(spec.Refs.Pulls) > 0 {
		kind = metapipeline.PullRequestPipeline
	} else {
		kind = metapipeline.ReleasePipeline
	}

	l := logrus.WithFields(logrus.Fields(map[string]interface{}{
		"Owner":     owner,
		"Name":      name,
		"SourceURL": sourceURL,
		"Branch":    branch,
		"PullRefs":  pullRefs,
		"Job":       job,
	}))
	l.Info("about to start Jenkinx X meta pipeline")

	sa := os.Getenv("JX_SERVICE_ACCOUNT")
	if sa == "" {
		sa = "tekton-bot"
	}

	pipelineCreateParam := metapipeline.PipelineCreateParam{
		PullRef:      pullRefData,
		PipelineKind: kind,
		Context:      spec.Context,
		// No equivalent to https://github.com/jenkins-x/jx/blob/bb59278c2707e0e99b3c24be926745c324824388/pkg/cmd/controller/pipeline/pipelinerunner_controller.go#L236
		//   for getting environment variables from the prow job here, so far as I can tell (abayer)
		// Also not finding an equivalent to labels from the PipelineRunRequest
		ServiceAccount: sa,
		// I believe we can use an empty string default image?
		DefaultImage: os.Getenv("JX_DEFAULT_IMAGE"),
		EnvVariables: spec.GetEnvVars(),
	}

	activityKey, tektonCRDs, err := metapipelineClient.Create(pipelineCreateParam)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create Tekton CRDs")
	}

	// Add the build number from the activity key to the labels on the job
	request.Labels[util.BuildNumLabel] = activityKey.Build

	appliedJob, err := b.lhClient.LighthouseV1alpha1().LighthouseJobs(b.namespace).Create(request)
	if err != nil {
		return nil, errors.Wrap(err, "unable to apply LighthouseJob")
	}

	// Set status on the job
	appliedJob.Status = v1alpha1.LighthouseJobStatus{
		State:        v1alpha1.PendingState,
		ActivityName: util.ToValidName(activityKey.Name),
		StartTime:    metav1.Now(),
	}
	fullyCreatedJob, err := b.lhClient.LighthouseV1alpha1().LighthouseJobs(b.namespace).UpdateStatus(appliedJob)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to set status on LighthouseJob %s", appliedJob.Name)
	}

	err = metapipelineClient.Apply(activityKey, tektonCRDs)
	if err != nil {
		return nil, errors.Wrap(err, "unable to apply Tekton CRDs")
	}
	return fullyCreatedJob, nil
}

func (b *launcher) getPullRefs(sourceURL string, spec *v1alpha1.LighthouseJobSpec) metapipeline.PullRef {
	var pullRef metapipeline.PullRef
	if len(spec.Refs.Pulls) > 0 {
		var prs []metapipeline.PullRequestRef
		for _, pull := range spec.Refs.Pulls {
			prs = append(prs, metapipeline.PullRequestRef{ID: strconv.Itoa(pull.Number), MergeSHA: pull.SHA})
		}

		pullRef = metapipeline.NewPullRefWithPullRequest(sourceURL, spec.Refs.BaseRef, spec.Refs.BaseSHA, prs...)
	} else {
		pullRef = metapipeline.NewPullRef(sourceURL, spec.Refs.BaseRef, spec.Refs.BaseSHA)
	}

	return pullRef
}
