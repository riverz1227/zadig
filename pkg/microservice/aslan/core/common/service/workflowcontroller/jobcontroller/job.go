/*
Copyright 2022 The KodeRover Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package jobcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/koderover/zadig/pkg/microservice/aslan/config"
	commonmodels "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	commonrepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb"
	"github.com/koderover/zadig/pkg/util/rand"
)

type JobCtl interface {
	Run(ctx context.Context)
	// do some clean stuff when workflow finished, like collect reports or clean up resources.
	Clean(ctx context.Context)
	// SaveInfo is used to update the basic information of the job task to the mongoDB
	SaveInfo(ctx context.Context) error
}

func initJobCtl(job *commonmodels.JobTask, workflowCtx *commonmodels.WorkflowTaskCtx, logger *zap.SugaredLogger, ack func()) JobCtl {
	var jobCtl JobCtl
	switch job.JobType {
	case string(config.JobZadigDeploy):
		jobCtl = NewDeployJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobZadigHelmDeploy):
		jobCtl = NewHelmDeployJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobCustomDeploy):
		jobCtl = NewCustomDeployJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobPlugin):
		jobCtl = NewPluginsJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobK8sCanaryDeploy):
		jobCtl = NewCanaryDeployJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobK8sCanaryRelease):
		jobCtl = NewCanaryReleaseJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobK8sBlueGreenDeploy):
		jobCtl = NewBlueGreenDeployJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobK8sBlueGreenRelease):
		jobCtl = NewBlueGreenReleaseJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobK8sGrayRelease):
		jobCtl = NewGrayReleaseJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobK8sGrayRollback):
		jobCtl = NewGrayRollbackJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobK8sPatch):
		jobCtl = NewK8sPatchJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobIstioRelease):
		jobCtl = NewIstioReleaseJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobIstioRollback):
		jobCtl = NewIstioRollbackJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobJira):
		jobCtl = NewJiraJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobNacos):
		jobCtl = NewNacosJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobApollo):
		jobCtl = NewApolloJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobMeegoTransition):
		jobCtl = NewMeegoTransitionJobCtl(job, workflowCtx, ack, logger)
	case string(config.JobWorkflowTrigger):
		jobCtl = NewWorkflowTriggerJobCtl(job, workflowCtx, ack, logger)
	default:
		jobCtl = NewFreestyleJobCtl(job, workflowCtx, ack, logger)
	}
	return jobCtl
}

func runJob(ctx context.Context, job *commonmodels.JobTask, workflowCtx *commonmodels.WorkflowTaskCtx, logger *zap.SugaredLogger, ack func()) {
	// should skip passed job when workflow task be restarted
	if job.Status == config.StatusPassed {
		return
	}
	// render global variables for every job.
	workflowCtx.GlobalContextEach(func(k, v string) bool {
		b, _ := json.Marshal(job)
		v = strings.Trim(v, "\n")
		replacedString := strings.ReplaceAll(string(b), k, v)
		if err := json.Unmarshal([]byte(replacedString), &job); err != nil {
			logger.Errorf("unmarshal job error: %v", err)
		}
		return true
	})
	job.Status = config.StatusPrepare
	job.StartTime = time.Now().Unix()
	job.K8sJobName = getJobName(workflowCtx.WorkflowName, workflowCtx.TaskID)
	ack()

	logger.Infof("start job: %s,status: %s", job.Name, job.Status)
	jobCtl := initJobCtl(job, workflowCtx, logger, ack)
	defer func(jobInfo *JobCtl) {
		if err := recover(); err != nil {
			errMsg := fmt.Sprintf("job: %s panic: %v", job.Name, err)
			logger.Error(errMsg)
			job.Status = config.StatusFailed
			job.Error = errMsg
		}
		job.EndTime = time.Now().Unix()
		logger.Infof("finish job: %s,status: %s", job.Name, job.Status)
		ack()
		logger.Infof("updating job info into db...")
		err := jobCtl.SaveInfo(ctx)
		if err != nil {
			logger.Errorf("update job info: %s into db error: %v", err)
		}
	}(&jobCtl)

	jobCtl.Run(ctx)
}

func RunJobs(ctx context.Context, jobs []*commonmodels.JobTask, workflowCtx *commonmodels.WorkflowTaskCtx, concurrency int, logger *zap.SugaredLogger, ack func()) {
	if concurrency == 1 {
		for _, job := range jobs {
			runJob(ctx, job, workflowCtx, logger, ack)
			if jobStatusFailed(job.Status) {
				return
			}
		}
		return
	}
	jobPool := NewPool(ctx, jobs, workflowCtx, concurrency, logger, ack)
	jobPool.Run()
}

func CleanWorkflowJobs(ctx context.Context, workflowTask *commonmodels.WorkflowTask, workflowCtx *commonmodels.WorkflowTaskCtx, logger *zap.SugaredLogger, ack func()) {
	for _, stage := range workflowTask.Stages {
		for _, job := range stage.Jobs {
			jobCtl := initJobCtl(job, workflowCtx, logger, ack)
			jobCtl.Clean(ctx)
		}
	}
}

// Pool is a worker group that runs a number of tasks at a
// configured concurrency.
type Pool struct {
	Jobs        []*commonmodels.JobTask
	workflowCtx *commonmodels.WorkflowTaskCtx
	concurrency int
	jobsChan    chan *commonmodels.JobTask
	logger      *zap.SugaredLogger
	ack         func()
	ctx         context.Context
	wg          sync.WaitGroup
}

// NewPool initializes a new pool with the given tasks and
// at the given concurrency.
func NewPool(ctx context.Context, jobs []*commonmodels.JobTask, workflowCtx *commonmodels.WorkflowTaskCtx, concurrency int, logger *zap.SugaredLogger, ack func()) *Pool {
	return &Pool{
		Jobs:        jobs,
		concurrency: concurrency,
		workflowCtx: workflowCtx,
		jobsChan:    make(chan *commonmodels.JobTask),
		logger:      logger,
		ack:         ack,
		ctx:         ctx,
	}
}

// Run runs all job within the pool and blocks until it's
// finished.
func (p *Pool) Run() {
	for i := 0; i < p.concurrency; i++ {
		go p.work()
	}

	p.wg.Add(len(p.Jobs))
	for _, task := range p.Jobs {
		p.jobsChan <- task
	}

	// all workers return
	close(p.jobsChan)

	p.wg.Wait()
}

// The work loop for any single goroutine.
func (p *Pool) work() {
	for job := range p.jobsChan {
		runJob(p.ctx, job, p.workflowCtx, p.logger, p.ack)
		p.wg.Done()
	}
}

func saveFile(src io.Reader, localFile string) error {
	out, err := os.Create(localFile)
	if err != nil {
		return err
	}

	defer out.Close()

	_, err = io.Copy(out, src)
	return err
}

func getJobName(workflowName string, taskID int64) string {
	// max lenth of workflowName was 32, so job name was unique in one task.
	base := strings.Replace(
		strings.ToLower(
			fmt.Sprintf(
				"%s-%d-",
				workflowName,
				taskID,
			),
		),
		"_", "-", -1,
	)
	return rand.GenerateName(base)
}

func jobStatusFailed(status config.Status) bool {
	if status == config.StatusCancelled || status == config.StatusFailed || status == config.StatusTimeout || status == config.StatusReject {
		return true
	}
	return false
}

func logError(job *commonmodels.JobTask, msg string, logger *zap.SugaredLogger) {
	logger.Error(msg)
	job.Status = config.StatusFailed
	job.Error = msg
}

// update product image info
func updateProductImageByNs(namespace, productName, serviceName string, targets map[string]string, logger *zap.SugaredLogger) error {
	opt := &commonrepo.ProductEnvFindOptions{
		Name:      productName,
		Namespace: namespace,
	}

	prod, err := commonrepo.NewProductColl().FindEnv(opt)

	if err != nil {
		logger.Errorf("find product namespace error: %v", err)
		return err
	}

	for i, group := range prod.Services {
		for j, service := range group {
			if service.ServiceName == serviceName {
				for l, container := range service.Containers {
					if image, ok := targets[container.Name]; ok {
						prod.Services[i][j].Containers[l].Image = image
					}
				}
			}
		}
	}

	if err := commonrepo.NewProductColl().Update(prod); err != nil {
		errMsg := fmt.Sprintf("[%s][%s] update product image error: %v", prod.EnvName, prod.ProductName, err)
		logger.Errorf(errMsg)
		return errors.New(errMsg)
	}

	return nil
}

func getMatchedRegistries(image string, registries []*commonmodels.RegistryNamespace) []*commonmodels.RegistryNamespace {
	resp := []*commonmodels.RegistryNamespace{}
	for _, registry := range registries {
		registryPrefix := registry.RegAddr
		if len(registry.Namespace) > 0 {
			registryPrefix = fmt.Sprintf("%s/%s", registry.RegAddr, registry.Namespace)
		}
		registryPrefix = strings.TrimPrefix(registryPrefix, "http://")
		registryPrefix = strings.TrimPrefix(registryPrefix, "https://")
		if strings.HasPrefix(image, registryPrefix) {
			resp = append(resp, registry)
		}
	}
	return resp
}
