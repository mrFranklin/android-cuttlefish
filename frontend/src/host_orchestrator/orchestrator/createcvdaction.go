// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package orchestrator

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/android-cuttlefish/frontend/src/host_orchestrator/orchestrator/artifacts"
	"github.com/google/android-cuttlefish/frontend/src/host_orchestrator/orchestrator/cvd"
	apiv1 "github.com/google/android-cuttlefish/frontend/src/liboperator/api/v1"
	"github.com/google/android-cuttlefish/frontend/src/liboperator/operator"

	"github.com/hashicorp/go-multierror"
)

type CreateCVDActionOpts struct {
	Request                  *apiv1.CreateCVDRequest
	HostValidator            Validator
	Paths                    IMPaths
	OperationManager         OperationManager
	ExecContext              ExecContext
	CVDDownloader            CVDDownloader
	CVDToolsVersion          AndroidBuild
	BuildAPI                 artifacts.BuildAPI
	ArtifactsFetcher         artifacts.Fetcher
	CVDBundleFetcher         artifacts.CVDBundleFetcher
	UUIDGen                  func() string
	CVDUser                  string
	CVDStartTimeout          time.Duration
	UserArtifactsDirResolver UserArtifactsDirResolver
	BuildAPICredentials      string
}

type CreateCVDAction struct {
	req                      *apiv1.CreateCVDRequest
	hostValidator            Validator
	paths                    IMPaths
	om                       OperationManager
	execContext              cvd.CVDExecContext
	cvdToolsVersion          AndroidBuild
	cvdDownloader            CVDDownloader
	buildAPI                 artifacts.BuildAPI
	artifactsFetcher         artifacts.Fetcher
	cvdBundleFetcher         artifacts.CVDBundleFetcher
	userArtifactsDirResolver UserArtifactsDirResolver
	artifactsMngr            *artifacts.Manager
	startCVDHandler          *startCVDHandler
	cvdUser                  string
	cvdStartTimeout          time.Duration
	buildAPICredentials      string

	instanceCounter uint32
}

func NewCreateCVDAction(opts CreateCVDActionOpts) *CreateCVDAction {
	cvdExecContext := newCVDExecContext(opts.ExecContext, opts.CVDUser)
	return &CreateCVDAction{
		req:                      opts.Request,
		hostValidator:            opts.HostValidator,
		paths:                    opts.Paths,
		om:                       opts.OperationManager,
		cvdToolsVersion:          opts.CVDToolsVersion,
		cvdDownloader:            opts.CVDDownloader,
		buildAPI:                 opts.BuildAPI,
		artifactsFetcher:         opts.ArtifactsFetcher,
		cvdBundleFetcher:         opts.CVDBundleFetcher,
		userArtifactsDirResolver: opts.UserArtifactsDirResolver,
		cvdUser:                  opts.CVDUser,
		cvdStartTimeout:          opts.CVDStartTimeout,
		buildAPICredentials:      opts.BuildAPICredentials,

		artifactsMngr: artifacts.NewManager(
			opts.Paths.ArtifactsRootDir,
			opts.UUIDGen,
		),
		execContext: cvdExecContext,
		startCVDHandler: &startCVDHandler{
			ExecContext: cvdExecContext,
			CVDBin:      opts.Paths.CVDBin(),
			Timeout:     opts.CVDStartTimeout,
		},
	}
}

func (a *CreateCVDAction) Run() (apiv1.Operation, error) {
	if err := validateRequest(a.req); err != nil {
		return apiv1.Operation{}, operator.NewBadRequestError("invalid CreateCVDRequest", err)
	}
	if err := a.hostValidator.Validate(); err != nil {
		return apiv1.Operation{}, err
	}
	if err := createDir(a.paths.ArtifactsRootDir); err != nil {
		return apiv1.Operation{}, err
	}
	if err := a.cvdDownloader.Download(a.cvdToolsVersion, a.paths.CVDBin(), a.paths.FetchCVDBin()); err != nil {
		return apiv1.Operation{}, err
	}
	op := a.om.New()
	go a.launchCVD(op)
	return op, nil
}

func (a *CreateCVDAction) launchCVD(op apiv1.Operation) {
	result := &OperationResult{}
	if a.req.EnvConfig != nil {
		result.Value, result.Error = a.launchWithCanonicalConfig(op)
	} else {
		result = a.launchCVDResult(op)
	}
	if err := a.om.Complete(op.Name, result); err != nil {
		log.Printf("error completing launch cvd operation %q: %v\n", op.Name, err)
	}
}

func (a *CreateCVDAction) launchWithCanonicalConfig(op apiv1.Operation) (*apiv1.CreateCVDResponse, error) {
	data, err := json.MarshalIndent(a.req.EnvConfig, "", " ")
	if err != nil {
		return nil, err
	}
	configFile, err := createTempFile("cvdload*.json", data, 0640)
	if err != nil {
		return nil, err
	}
	args := []string{"load", configFile.Name()}
	if a.buildAPICredentials != "" {
		filename, err := createCredsFile(a.execContext)
		if err != nil {
			return nil, err
		}
		if err := writeCredsFile(a.execContext, filename, []byte(a.buildAPICredentials)); err != nil {
			return nil, err
		}
		defer func() {
			if err := removeCredsFile(a.execContext, filename); err != nil {
				log.Println("failed to remove credentials file: ", err)
			}
		}()
		args = append(args, "--credential_source="+filename)
	}
	opts := cvd.CommandOpts{
		Timeout: a.cvdStartTimeout,
	}
	cmd := cvd.NewCommand(a.execContext, a.paths.CVDBin(), args, opts)
	if err := cmd.Run(); err != nil {
		return nil, operator.NewInternalError(ErrMsgLaunchCVDFailed, err)
	}
	group, err := cvdFleetFirstGroup(a.execContext, a.paths.CVDBin())
	if err != nil {
		return nil, err
	}
	return &apiv1.CreateCVDResponse{CVDs: group.toAPIObject()}, nil
}

func (a *CreateCVDAction) launchCVDResult(op apiv1.Operation) *OperationResult {
	instancesCount := 1 + a.req.AdditionalInstancesNum
	var instanceNumbers []uint32
	var err error
	switch {
	case a.req.CVD.BuildSource.AndroidCIBuildSource != nil:
		instanceNumbers, err = a.launchFromAndroidCI(a.req.CVD.BuildSource.AndroidCIBuildSource, instancesCount, op)
	case a.req.CVD.BuildSource.UserBuildSource != nil:
		instanceNumbers, err = a.launchFromUserBuild(a.req.CVD.BuildSource.UserBuildSource, instancesCount, op)
	default:
		return &OperationResult{
			Error: operator.NewBadRequestError(
				"Invalid CreateCVDRequest, missing BuildSource information.", nil),
		}
	}
	if err != nil {
		return &OperationResult{Error: operator.NewInternalError(ErrMsgLaunchCVDFailed, err)}
	}
	group, err := cvdFleetFirstGroup(a.execContext, a.paths.CVDBin())
	if err != nil {
		return &OperationResult{Error: operator.NewInternalError(ErrMsgLaunchCVDFailed, err)}
	}
	relevant := []*cvdInstance{}
	for _, item := range group.Instances {
		n, err := strconv.Atoi(item.InstanceName)
		if err != nil {
			return &OperationResult{Error: operator.NewInternalError(ErrMsgLaunchCVDFailed, err)}
		}
		if contains(instanceNumbers, uint32(n)) {
			relevant = append(relevant, item)
		}
	}
	group.Instances = relevant
	res := &apiv1.CreateCVDResponse{CVDs: group.toAPIObject()}
	return &OperationResult{Value: res}
}

const ErrMsgLaunchCVDFailed = "failed to launch cvd"

func (a *CreateCVDAction) launchFromAndroidCI(
	buildSource *apiv1.AndroidCIBuildSource, instancesCount uint32, op apiv1.Operation) ([]uint32, error) {
	var mainBuild *apiv1.AndroidCIBuild = defaultMainBuild()
	var kernelBuild *apiv1.AndroidCIBuild
	var bootloaderBuild *apiv1.AndroidCIBuild
	var systemImgBuild *apiv1.AndroidCIBuild
	if buildSource.MainBuild != nil {
		*mainBuild = *buildSource.MainBuild
	}
	if buildSource.KernelBuild != nil {
		kernelBuild = &apiv1.AndroidCIBuild{}
		*kernelBuild = *buildSource.KernelBuild
	}
	if buildSource.BootloaderBuild != nil {
		bootloaderBuild = &apiv1.AndroidCIBuild{}
		*bootloaderBuild = *buildSource.BootloaderBuild
	}
	if buildSource.SystemImageBuild != nil {
		systemImgBuild = &apiv1.AndroidCIBuild{}
		*systemImgBuild = *buildSource.SystemImageBuild
	}
	if err := updateBuildsWithLatestGreenBuildID(a.buildAPI,
		[]*apiv1.AndroidCIBuild{mainBuild, kernelBuild, bootloaderBuild, systemImgBuild}); err != nil {
		return nil, err
	}
	var mainBuildDir, kernelBuildDir, bootloaderBuildDir string
	var mainBuildErr, kernelBuildErr, bootloaderBuildErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var extraCVDOptions *artifacts.ExtraCVDOptions = nil
		if systemImgBuild != nil {
			extraCVDOptions = &artifacts.ExtraCVDOptions{
				SystemImgBuildID: systemImgBuild.BuildID,
				SystemImgTarget:  systemImgBuild.Target,
			}
		}
		mainBuildDir, mainBuildErr = a.artifactsMngr.GetCVDBundle(
			mainBuild.BuildID, mainBuild.Target, extraCVDOptions, a.cvdBundleFetcher)
	}()
	if kernelBuild != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kernelBuildDir, kernelBuildErr = a.artifactsMngr.GetKernelBundle(
				kernelBuild.BuildID, kernelBuild.Target, a.artifactsFetcher)
		}()
	}
	if bootloaderBuild != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bootloaderBuildDir, bootloaderBuildErr = a.artifactsMngr.GetBootloaderBundle(
				bootloaderBuild.BuildID, bootloaderBuild.Target, a.artifactsFetcher)
		}()
	}
	wg.Wait()
	var merr error
	for _, err := range []error{mainBuildErr, kernelBuildErr, bootloaderBuildErr} {
		if err != nil {
			merr = multierror.Append(merr, err)
		}
	}
	if merr != nil {
		return nil, merr
	}
	startParams := startCVDParams{
		InstanceNumbers:  a.newInstanceNumbers(instancesCount),
		MainArtifactsDir: mainBuildDir,
		KernelDir:        kernelBuildDir,
		BootloaderDir:    bootloaderBuildDir,
	}
	if err := a.startCVDHandler.Start(startParams); err != nil {
		return nil, err
	}
	// TODO: Remove once `acloud CLI` gets deprecated.
	if contains(startParams.InstanceNumbers, 1) {
		go runAcloudSetup(a.execContext, a.paths.ArtifactsRootDir, mainBuildDir)
	}
	return startParams.InstanceNumbers, nil
}

func (a *CreateCVDAction) launchFromUserBuild(
	buildSource *apiv1.UserBuildSource, instancesCount uint32, op apiv1.Operation) ([]uint32, error) {
	artifactsDir := a.userArtifactsDirResolver.GetDirPath(buildSource.ArtifactsDir)
	if err := untarCVDHostPackage(artifactsDir); err != nil {
		return nil, err
	}
	// assemble_cvd needs writer permission over vbmeta images
	// https://cs.android.com/android/platform/superproject/main/+/main:device/google/cuttlefish/host/commands/assemble_cvd/disk_flags.cc;l=639;drc=b0ec6e4df1126fd4045ce32bbfcedb79f25bd5bc
	for _, name := range []string{"vbmeta.img", "vbmeta_system.img"} {
		if err := os.Chmod(filepath.Join(artifactsDir, name), 0664); err != nil {
			return nil, err
		}
	}
	startParams := startCVDParams{
		InstanceNumbers:  a.newInstanceNumbers(instancesCount),
		MainArtifactsDir: artifactsDir,
	}
	if err := a.startCVDHandler.Start(startParams); err != nil {
		return nil, err
	}
	// TODO: Remove once `acloud CLI` gets deprecated.
	if contains(startParams.InstanceNumbers, 1) {
		go runAcloudSetup(a.execContext, a.paths.ArtifactsRootDir, artifactsDir)
	}
	return startParams.InstanceNumbers, nil
}

func (a *CreateCVDAction) newInstanceNumbers(n uint32) []uint32 {
	result := []uint32{}
	for i := 0; i < int(n); i++ {
		num := atomic.AddUint32(&a.instanceCounter, 1)
		result = append(result, num)
	}
	return result
}

func validateRequest(r *apiv1.CreateCVDRequest) error {
	if r.EnvConfig != nil {
		return nil
	}
	if r.CVD == nil {
		return EmptyFieldError("CVD")
	}
	if r.CVD.BuildSource == nil {
		return EmptyFieldError("BuildSource")
	}
	if r.CVD.BuildSource.AndroidCIBuildSource == nil && r.CVD.BuildSource.UserBuildSource == nil {
		return EmptyFieldError("BuildSource")
	}
	if r.CVD.BuildSource.UserBuildSource != nil {
		if r.CVD.BuildSource.UserBuildSource.ArtifactsDir == "" {
			return EmptyFieldError("BuildSource.UserBuild.ArtifactsDir")
		}
	}
	return nil
}

// See https://pkg.go.dev/io/ioutil@go1.13.15#TempFile
func createTempFile(pattern string, data []byte, mode os.FileMode) (*os.File, error) {
	file, err := ioutil.TempFile("", pattern)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(mode); err != nil {
		return nil, err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return file, nil
}

// Create the credential file so it's owned by the configured `cvd user`, e.g: `_cvd-executor`.
func createCredsFile(ctx cvd.CVDExecContext) (string, error) {
	name, err := tempFilename("cvdload*.creds")
	if err != nil {
		return "", err
	}
	if err := cvd.Exec(ctx, "touch", name); err != nil {
		return "", err
	}
	if err := cvd.Exec(ctx, "chmod", "0600", name); err != nil {
		return "", err
	}
	return name, nil
}

// Write into credential files by granting temporary write permission to `cvdnetwork` group.
func writeCredsFile(ctx cvd.CVDExecContext, name string, data []byte) error {
	info, err := os.Stat(name)
	if err != nil {
		return err
	}
	infoSys := info.Sys()
	var gid uint32
	if stat, ok := infoSys.(*syscall.Stat_t); ok {
		gid = stat.Gid
	} else {
		panic("unexpected stat syscall type")
	}
	defer func() {
		// Reverts the write permission.
		if err := cvd.Exec(ctx, "chgrp", strconv.Itoa(int(gid)), name); err != nil {
			log.Println(err)
		}
		if err := cvd.Exec(ctx, "chmod", "0600", name); err != nil {
			log.Println(err)
		}
	}()
	// Grants temporal write permission to `cvdnetwork`, so this process can write the file.
	if err := cvd.Exec(ctx, "chgrp", "cvdnetwork", name); err != nil {
		return err
	}
	if err := cvd.Exec(ctx, "chmod", "0620", name); err != nil {
		return err
	}
	file, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return nil
}

func removeCredsFile(ctx cvd.CVDExecContext, name string) error {
	return cvd.Exec(ctx, "rm", name)
}

// Returns a random name for a file in the /tmp directory given a pattern.
// See https://pkg.go.dev/io/ioutil@go1.13.15#TempFile
func tempFilename(pattern string) (string, error) {
	file, err := ioutil.TempFile("", pattern)
	if err != nil {
		return "", err
	}
	name := file.Name()
	if os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}
