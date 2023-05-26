package nomadcluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	// types "github.com/hashicorp/nomad-openapi/clients/go/v1"
	// v1 "github.com/hashicorp/nomad-openapi/v1"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/jobspec2"

	"github.com/nomad-ops/nomad-ops/backend/application"
	"github.com/nomad-ops/nomad-ops/backend/domain"
	"github.com/nomad-ops/nomad-ops/backend/utils/log"
)

var (
	metaKeyOps          = "nomadops"
	metaKeySrcID        = "nomadopssrcid"
	metaKeySrcUrl       = "nomadopssrcurl"
	metaKeySrcCommit    = "nomadopssrccommit"
	metaKeyForceRestart = "nomadopsforcerestart"
)

type ClientConfig struct {
	NomadToken string
}

type Client struct {
	ctx    context.Context
	logger log.Logger
	cfg    ClientConfig
	client *api.Client
	url    string
}

func CreateClient(ctx context.Context,
	logger log.Logger,
	cfg ClientConfig) (*Client, error) {

	defCfg := api.DefaultConfig()

	if cfg.NomadToken != "" {
		// Use default client config from ENV, optionally a custom token
		defCfg.SecretID = cfg.NomadToken
	}

	client, err := api.NewClient(defCfg)

	if err != nil {
		return nil, err
	}

	c := &Client{
		ctx:    ctx,
		logger: logger,
		cfg:    cfg,
		client: client,
		url:    defCfg.Address,
	}

	return c, nil
}

func (c *Client) SubscribeJobChanges(ctx context.Context, cb func(jobName string)) error {
	var index uint64 = 0
	if _, meta, err := c.client.Jobs().List(nil); err == nil {
		index = meta.LastIndex
	}

	eventCh, err := c.client.EventStream().Stream(ctx, map[api.Topic][]string{
		api.TopicJob:        {"*"},
		api.TopicDeployment: {"*"},
	}, index, &api.QueryOptions{
		Namespace: "*",
	})
	if err != nil {
		return err
	}

	eventHandler := func(event *api.Events) {
		for _, e := range event.Events {

			c.logger.LogInfo(ctx, "Received nomad event:%v", e.Type)

			switch e.Type {
			case "JobRegistered", "JobDeregistered":

				job, err := e.Job()
				if err != nil {
					return
				}

				cb(*job.ID)
			case "DeploymentStatusUpdate":
				dep, err := e.Deployment()
				if err != nil {
					return
				}
				cb(dep.JobID)
			default:
			}
		}
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return

			case events := <-eventCh:

				if events.IsHeartbeat() {
					continue
				}

				eventHandler(events)
			}
		}
	}()

	return nil
}

func hasUpdate(diffResp *api.JobPlanResponse, restart, force bool) bool {
	hasDiff := false
	if len(diffResp.Diff.Objects) > 0 {
		return true
	}
	fieldDiff := diffResp.Diff.Fields
	if len(fieldDiff) > 0 {
		// if only the git commit change we will not see it as a change
		// if only the forced restart is a change we will not see it as a change either
		// use force to update it anyway
		if len(fieldDiff) != 1 ||
			(fieldDiff[0].Name != fmt.Sprintf("Meta[%s]", metaKeySrcCommit) &&
				fieldDiff[0].Name != fmt.Sprintf("Meta[%s]", metaKeyForceRestart)) ||
			force || restart {
			return true
		}
	}
	for _, taskGrp := range diffResp.Diff.TaskGroups {
		if len(taskGrp.Fields) > 0 {
			return true
		}
		if len(taskGrp.Objects) > 0 {
			return true
		}

		for _, task := range taskGrp.Tasks {
			if len(task.Fields) > 0 {
				return true
			}
			if len(task.Objects) > 0 {
				return true
			}
		}
	}
	return hasDiff
}

func (c *Client) ParseJob(ctx context.Context, j string) (*application.JobInfo, error) {

	pJob, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{
		Path:    "",
		Body:    []byte(j),
		AllowFS: true,
		ArgVars: nil,
		Strict:  true,
	})
	if err != nil {
		return nil, err
	}

	return &application.JobInfo{
		Job: pJob,
	}, nil
}

func (c *Client) getQueryOptsCtx(ctx context.Context, src *domain.Source) *api.QueryOptions {

	opts := api.QueryOptions{}
	if src.Namespace != "" {
		opts.Namespace = src.Namespace
	}
	if src.Region != "" {
		opts.Region = src.Region
	}

	return &opts
}

func (c *Client) getWriteOptions(ctx context.Context, src *domain.Source) *api.WriteOptions {

	opts := api.WriteOptions{}
	if src.Namespace != "" {
		opts.Namespace = src.Namespace
	}
	if src.Region != "" {
		opts.Region = src.Region
	}

	return &opts
}

func (c *Client) UpdateJob(ctx context.Context,
	src *domain.Source,
	job *application.JobInfo,
	restart bool) (*application.UpdateJobInfo, error) {

	if src.CreateNamespace {
		if job.Namespace == nil {
			return nil, fmt.Errorf("require a namespace to be set in conjunction with 'CreateNamespace'")
		}
		// Make sure that namespace exists
		_, err := c.client.Namespaces().Register(&api.Namespace{
			Name: *job.Namespace,
			Meta: map[string]string{
				metaKeyOps: "true",
			},
		}, c.getWriteOptions(ctx, src))
		if err != nil {
			return nil, err
		}
	}

	metadata := job.Job.Meta
	if metadata == nil {
		metadata = map[string]string{}
	}

	// claiming this job as our job!
	metadata[metaKeyOps] = "true"
	metadata[metaKeySrcUrl] = src.URL
	metadata[metaKeySrcID] = src.ID
	metadata[metaKeySrcCommit] = job.GitInfo.GitCommit

	if restart {
		metadata[metaKeyForceRestart] = time.Now().Format(time.RFC3339Nano)
	}

	job.Meta = metadata
	resp, _, err := c.client.Jobs().Plan(job.Job, true, c.getWriteOptions(ctx, src))

	if err != nil {
		return nil, err
	}

	deploymentStatus := ""

	deployment, _, err := c.client.Jobs().LatestDeployment(*job.ID, c.getQueryOptsCtx(ctx, src))
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			// low effort "not found" detection
			return nil, err
		}
	}
	if deployment != nil {
		deploymentStatus = deployment.Status
		c.logger.LogInfo(ctx, "DeploymentStatus:%s %v", *job.ID, deploymentStatus)
	}

	if !hasUpdate(resp, restart, src.Force) {
		c.logger.LogTrace(ctx, "Job is already up to date.")

		return &application.UpdateJobInfo{
			DeploymentStatus: application.DeploymentStatus{
				Status: deploymentStatus,
			},
		}, nil
	}

	c.logger.LogInfo(ctx, "Job Diff:%v", log.ToJSONString(resp.Diff))

	if !src.Paused {
		regResp, _, err := c.client.Jobs().Register(job.Job, c.getWriteOptions(ctx, src))
		if err != nil {
			return nil, err
		}

		c.logger.LogInfo(ctx, "Job Post:%v", log.ToJSONString(regResp))
	}

	return &application.UpdateJobInfo{
		Updated: true, // TODO check for creation, for now everything is an update...which is kinda true
		DeploymentStatus: application.DeploymentStatus{
			Status: deploymentStatus,
		},
	}, nil
}

func (c *Client) DeleteJob(ctx context.Context, src *domain.Source, job *application.JobInfo) error {

	_, _, err := c.client.Jobs().Deregister(*job.Job.Name, false, c.getWriteOptions(ctx, src))

	if err != nil {
		return err
	}

	return nil
}

func (c *Client) GetURL(ctx context.Context) (string, error) {
	return c.url, nil
}

func (c *Client) GetCurrentClusterState(ctx context.Context,
	opts application.GetCurrentClusterStateOptions) (*application.ClusterState, error) {

	// TODO add filter to match only jobs with valid meta
	joblist, _, err := c.client.Jobs().List(c.getQueryOptsCtx(ctx, opts.Source))
	if err != nil {
		return nil, err
	}

	clusterState := &application.ClusterState{
		CurrentJobs: map[string]*application.JobInfo{},
	}

	for _, job := range joblist {
		j, _, err := c.client.Jobs().Info(job.Name, c.getQueryOptsCtx(ctx, opts.Source))
		if err != nil {
			return nil, err
		}

		m := j.Meta
		// Ignore stuff that is not managed by us
		if len(m) == 0 {
			continue
		}
		// only consider jobs with my source id!
		if m[metaKeySrcID] != opts.Source.ID {
			continue
		}

		clusterState.CurrentJobs[job.Name] = &application.JobInfo{
			Job: j,
		}
	}

	return clusterState, nil
}
