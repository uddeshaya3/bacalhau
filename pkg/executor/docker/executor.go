package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/filecoin-project/bacalhau/pkg/compute/capacity"
	"github.com/pkg/errors"
	"go.uber.org/multierr"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	dockerclient "github.com/docker/docker/client"
	"github.com/filecoin-project/bacalhau/pkg/config"
	"github.com/filecoin-project/bacalhau/pkg/docker"
	"github.com/filecoin-project/bacalhau/pkg/executor"
	jobutils "github.com/filecoin-project/bacalhau/pkg/job"
	"github.com/filecoin-project/bacalhau/pkg/model"
	"github.com/filecoin-project/bacalhau/pkg/storage"
	"github.com/filecoin-project/bacalhau/pkg/storage/util"
	"github.com/filecoin-project/bacalhau/pkg/system"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const NanoCPUCoefficient = 1000000000

const (
	labelExecutorName = "bacalhau-executor"
	labelJobName      = "bacalhau-jobID"
)

type Executor struct {
	// used to allow multiple docker executors to run against the same docker server
	ID string

	// the storage providers we can implement for a job
	StorageProvider storage.StorageProvider

	Client *dockerclient.Client
}

func NewExecutor(
	ctx context.Context,
	cm *system.CleanupManager,
	id string,
	storageProvider storage.StorageProvider,
) (*Executor, error) {
	dockerClient, err := docker.NewDockerClient()
	if err != nil {
		return nil, err
	}

	de := &Executor{
		ID:              id,
		StorageProvider: storageProvider,
		Client:          dockerClient,
	}

	cm.RegisterCallback(func() error {
		de.cleanupAll(ctx)
		return nil
	})

	return de, nil
}

func (e *Executor) getStorage(ctx context.Context, engine model.StorageSourceType) (storage.Storage, error) {
	return e.StorageProvider.GetStorage(ctx, engine)
}

// IsInstalled checks if docker itself is installed.
func (e *Executor) IsInstalled(ctx context.Context) (bool, error) {
	return docker.IsInstalled(ctx, e.Client), nil
}

func (e *Executor) HasStorageLocally(ctx context.Context, volume model.StorageSpec) (bool, error) {
	//nolint:ineffassign,staticcheck
	ctx, span := system.GetTracer().Start(ctx, "pkg/executor/docker/Executor.HasStorageLocally")
	defer span.End()

	s, err := e.getStorage(ctx, volume.StorageSource)
	if err != nil {
		return false, err
	}

	return s.HasStorageLocally(ctx, volume)
}

func (e *Executor) GetVolumeSize(ctx context.Context, volume model.StorageSpec) (uint64, error) {
	storageProvider, err := e.getStorage(ctx, volume.StorageSource)
	if err != nil {
		return 0, err
	}
	return storageProvider.GetVolumeSize(ctx, volume)
}

//nolint:funlen,gocyclo // will clean up
func (e *Executor) RunShard(
	ctx context.Context,
	shard model.JobShard,
	jobResultsDir string,
) (*model.RunCommandResult, error) {
	//nolint:ineffassign,staticcheck
	ctx, span := system.GetTracer().Start(ctx, "pkg/executor/docker.RunShard")
	defer span.End()
	system.AddJobIDFromBaggageToSpan(ctx, span)
	system.AddNodeIDFromBaggageToSpan(ctx, span)
	defer e.cleanupJob(ctx, shard)

	shardStorageSpec, err := jobutils.GetShardStorageSpec(ctx, shard, e.StorageProvider)
	if err != nil {
		return executor.FailResult(err)
	}

	var inputStorageSpecs []model.StorageSpec
	inputStorageSpecs = append(inputStorageSpecs, shard.Job.Spec.Contexts...)
	inputStorageSpecs = append(inputStorageSpecs, shardStorageSpec...)

	inputVolumes, err := storage.ParallelPrepareStorage(ctx, e.StorageProvider, inputStorageSpecs)
	if err != nil {
		return executor.FailResult(err)
	}

	// the actual mounts we will give to the container
	// these are paths for both input and output data
	var mounts []mount.Mount
	for spec, volumeMount := range inputVolumes {
		if volumeMount.Type == storage.StorageVolumeConnectorBind {
			log.Ctx(ctx).Trace().Msgf("Input Volume: %+v %+v", spec, volumeMount)
			mounts = append(mounts, mount.Mount{
				Type: mount.TypeBind,
				// this is an input volume so is read only
				ReadOnly: true,
				Source:   volumeMount.Source,
				Target:   volumeMount.Target,
			})
		} else {
			return executor.FailResult(fmt.Errorf("unknown storage volume type: %s", volumeMount.Type))
		}
	}

	// for this phase of the outputs we ignore the engine because it's just about collecting the
	// data from the job and keeping it locally
	// the engine property of the output storage spec is how we will "publish" the output volume
	// if and when the deal is settled
	for _, output := range shard.Job.Spec.Outputs {
		if output.Name == "" {
			err = fmt.Errorf("output volume has no name: %+v", output)
			return executor.FailResult(err)
		}

		if output.Path == "" {
			err = fmt.Errorf("output volume has no path: %+v", output)
			return executor.FailResult(err)
		}

		srcd := filepath.Join(jobResultsDir, output.Name)
		err = os.Mkdir(srcd, util.OS_ALL_R|util.OS_ALL_X|util.OS_USER_W)
		if err != nil {
			return executor.FailResult(err)
		}

		log.Ctx(ctx).Trace().Msgf("Output Volume: %+v", output)

		// create a mount so the output data does not need to be copied back to the host
		mounts = append(mounts, mount.Mount{

			Type: mount.TypeBind,
			// this is an output volume so can be written to
			ReadOnly: false,

			// we create a named folder in the job results folder for this output
			Source: srcd,

			// the path of the output volume is from the perspective of inside the container
			Target: output.Path,
		})
	}

	if os.Getenv("SKIP_IMAGE_PULL") == "" {
		if err := docker.PullImage(ctx, e.Client, shard.Job.Spec.Docker.Image); err != nil { //nolint:govet // ignore err shadowing
			err = errors.Wrapf(err, `Could not pull image %q - could be due to repo/image not existing,
 or registry needing authorization`, shard.Job.Spec.Docker.Image)
			return executor.FailResult(err)
		}
	}

	// json the job spec and pass it into all containers
	// TODO: check if this will overwrite a user supplied version of this value
	// (which is what we actually want to happen)
	log.Ctx(ctx).Debug().Msgf("Job Spec: %+v", shard.Job.Spec)
	jsonJobSpec, err := model.JSONMarshalWithMax(shard.Job.Spec)
	if err != nil {
		return executor.FailResult(err)
	}
	log.Ctx(ctx).Debug().Msgf("Job Spec JSON: %s", jsonJobSpec)

	useEnv := append(shard.Job.Spec.Docker.EnvironmentVariables,
		fmt.Sprintf("BACALHAU_JOB_SPEC=%s", string(jsonJobSpec)),
	)

	containerConfig := &container.Config{
		Image:      shard.Job.Spec.Docker.Image,
		Tty:        false,
		Env:        useEnv,
		Entrypoint: shard.Job.Spec.Docker.Entrypoint,
		Labels:     e.jobContainerLabels(shard),
		WorkingDir: shard.Job.Spec.Docker.WorkingDirectory,
	}

	log.Ctx(ctx).Trace().Msgf("Container: %+v %+v", containerConfig, mounts)

	resourceRequirements := capacity.ParseResourceUsageConfig(shard.Job.Spec.Resources)

	// Create GPU request if the job requests it
	var deviceRequests []container.DeviceRequest
	if resourceRequirements.GPU > 0 {
		deviceRequests = append(deviceRequests,
			container.DeviceRequest{
				DeviceIDs:    []string{"0"}, // TODO: how do we know which device ID to use?
				Capabilities: [][]string{{"gpu"}},
			},
		)
		log.Ctx(ctx).Trace().Msgf("Adding %d GPUs to request", resourceRequirements.GPU)
	}

	hostConfig := &container.HostConfig{
		Mounts: mounts,
		Resources: container.Resources{
			Memory:         int64(resourceRequirements.Memory),
			NanoCPUs:       int64(resourceRequirements.CPU * NanoCPUCoefficient),
			DeviceRequests: deviceRequests,
		},
	}

	// Create a network if the job requests it
	err = e.setupNetworkForJob(ctx, shard, containerConfig, hostConfig)
	if err != nil {
		return executor.FailResult(err)
	}

	jobContainer, err := e.Client.ContainerCreate(
		ctx,
		containerConfig,
		hostConfig,
		nil,
		nil,
		e.jobContainerName(shard),
	)
	if err != nil {
		return executor.FailResult(errors.Wrap(err, "failed to create container"))
	}

	ctx = log.Ctx(ctx).With().Str("Container", jobContainer.ID).Logger().WithContext(ctx)

	containerStartError := e.Client.ContainerStart(
		ctx,
		jobContainer.ID,
		dockertypes.ContainerStartOptions{},
	)
	if containerStartError != nil {
		// Special error to alert people about bad executable
		internalContainerStartErrorMsg := "failed to start container"
		if strings.Contains(containerStartError.Error(), "executable file not found") {
			internalContainerStartErrorMsg = "Executable file not found"
		}
		internalContainerStartError := errors.Wrap(containerStartError, internalContainerStartErrorMsg)
		return executor.FailResult(internalContainerStartError)
	}

	log.Ctx(ctx).Debug().Msg("Capturing stdout/stderr for container")
	stdoutPipe, stderrPipe, logsErr := docker.FollowLogs(ctx, e.Client, jobContainer.ID)

	// the idea here is even if the container errors
	// we want to capture stdout, stderr and feed it back to the user
	var containerError error
	var containerExitStatusCode int64
	statusCh, errCh := e.Client.ContainerWait(
		ctx,
		jobContainer.ID,
		container.WaitConditionNotRunning,
	)
	select {
	case err = <-errCh:
		containerError = err
	case exitStatus := <-statusCh:
		containerExitStatusCode = exitStatus.StatusCode
		if exitStatus.Error != nil {
			containerError = errors.New(exitStatus.Error.Message)
		}
	}

	return executor.WriteJobResults(
		jobResultsDir,
		stdoutPipe,
		stderrPipe,
		int(containerExitStatusCode),
		multierr.Combine(containerError, logsErr),
	)
}

func (e *Executor) CancelShard(ctx context.Context, shard model.JobShard) error {
	return docker.RemoveObjectsWithLabel(ctx, e.Client, labelJobName, e.labelJobValue(shard))
}

func (e *Executor) cleanupJob(ctx context.Context, shard model.JobShard) {
	if config.ShouldKeepStack() {
		return
	}

	// Use a separate context in case the current one has already been canceled
	separateCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	err := docker.RemoveObjectsWithLabel(separateCtx, e.Client, labelJobName, e.labelJobValue(shard))
	logLevel := map[bool]zerolog.Level{true: zerolog.DebugLevel, false: zerolog.ErrorLevel}[err == nil]
	log.Ctx(ctx).WithLevel(logLevel).Err(err).Msg("Cleaned up job Docker resources")
}

func (e *Executor) cleanupAll(ctx context.Context) {
	if config.ShouldKeepStack() {
		return
	}

	// We have to use a separate context, rather than the one passed in to `NewExecutor`, as it may have already been
	// canceled and so would prevent us from performing any cleanup work.
	safeCtx := context.Background()

	err := docker.RemoveObjectsWithLabel(safeCtx, e.Client, labelExecutorName, e.ID)
	logLevel := map[bool]zerolog.Level{true: zerolog.DebugLevel, false: zerolog.ErrorLevel}[err == nil]
	log.Ctx(ctx).WithLevel(logLevel).Err(err).Msg("Cleaned up all Docker resources")
}

func (e *Executor) dockerObjectName(shard model.JobShard, parts ...string) string {
	strs := []string{"bacalhau", e.ID, shard.Job.Metadata.ID, fmt.Sprint(shard.Index)}
	strs = append(strs, parts...)
	return strings.Join(strs, "-")
}

func (e *Executor) jobContainerName(shard model.JobShard) string {
	return e.dockerObjectName(shard, "executor")
}

func (e *Executor) jobContainerLabels(shard model.JobShard) map[string]string {
	return map[string]string{
		labelExecutorName: e.ID,
		labelJobName:      e.labelJobValue(shard),
	}
}

func (e *Executor) labelJobValue(shard model.JobShard) string {
	return e.ID + shard.ID()
}

// Compile-time interface check:
var _ executor.Executor = (*Executor)(nil)
