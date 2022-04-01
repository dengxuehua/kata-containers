// Copyright (c) 2018 HyperHQ Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// template implements base vm factory with vm templating.

package template

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	pb "github.com/kata-containers/kata-containers/src/runtime/protocols/cache"
	vc "github.com/kata-containers/kata-containers/src/runtime/virtcontainers"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/factory/base"
	"github.com/sirupsen/logrus"
)

type template struct {
	statePath string
	config    vc.VMConfig
}

var templateWaitForAgent = 2 * time.Second
var templateLog = logrus.WithField("source", "virtcontainers/factory/template")

const (
	clhStateFile = "state.json"
)

// Fetch finds and returns a pre-built template factory.
// TODO: save template metadata and fetch from storage.
func Fetch(config vc.VMConfig, templatePath string) (base.FactoryBase, error) {
	t := &template{templatePath, config}

	err := t.checkTemplateVM()
	if err != nil {
		return nil, err
	}

	return t, nil
}

// New creates a new VM template factory.
func New(ctx context.Context, config vc.VMConfig, templatePath string) (base.FactoryBase, error) {
	t := &template{templatePath, config}

	err := t.checkTemplateVM()
	if err == nil {
		return nil, fmt.Errorf("There is already a VM template in %s", templatePath)
	}

	err = t.prepareTemplateFiles()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			t.close()
		}
	}()

	err = t.createTemplateVM(ctx)
	if err != nil {
		return nil, err
	}

	return t, nil
}

// Config returns template factory's configuration.
func (t *template) Config() vc.VMConfig {
	return t.config
}

// GetBaseVM creates a new paused VM from the template VM.
func (t *template) GetBaseVM(ctx context.Context, config vc.VMConfig) (*vc.VM, error) {
	return t.createFromTemplateVM(ctx, config)
}

// CloseFactory cleans up the template VM.
func (t *template) CloseFactory(ctx context.Context) {
	t.close()
}

// GetVMStatus is not supported
func (t *template) GetVMStatus() []*pb.GrpcVMStatus {
	panic("ERROR: package template does not support GetVMStatus")
}

func (t *template) close() {
	if err := syscall.Unmount(t.statePath, syscall.MNT_DETACH); err != nil {
		t.Logger().WithError(err).Errorf("failed to unmount %s", t.statePath)
	}

	if err := os.RemoveAll(t.statePath); err != nil {
		t.Logger().WithError(err).Errorf("failed to remove %s", t.statePath)
	}
}

func (t *template) prepareTemplateFiles() error {
	// create and mount tmpfs for the shared memory file
	err := os.MkdirAll(t.statePath, 0700)
	if err != nil {
		return err
	}
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
	opts := fmt.Sprintf("size=%dM", t.config.HypervisorConfig.MemorySize+templateDeviceStateSize)
	if err = syscall.Mount("tmpfs", t.statePath, "tmpfs", flags, opts); err != nil {
		t.close()
		return err
	}
	f, err := os.Create(t.statePath + "/memory")
	if err != nil {
		t.close()
		return err
	}
	f.Close()

	return nil
}

func (t *template) createTemplateVM(ctx context.Context) error {
	// create the template vm
	config := t.config
	config.HypervisorConfig.BootToBeTemplate = true
	config.HypervisorConfig.BootFromTemplate = false
	config.HypervisorConfig.MemoryPath = t.statePath + "/memory"
	config.HypervisorConfig.DevicesStatePath = t.statePath + "/state"

	if t.config.HypervisorType == vc.ClhHypervisor {
		config.HypervisorConfig.DevicesStatePath = t.statePath + "/" + clhStateFile
		// Cloud Hypervisor use "hybrid VSOCK" which uses a local
		// UNIX socket rather than the host kernel to handle communication with the
		// guest. As such, we need to specify the path to the UNIX socket.
		config.HypervisorConfig.VMStorePath = t.statePath
	}

	vm, err := vc.NewVM(ctx, config)
	if err != nil {
		return err
	}
	defer vm.Stop(ctx)

	if err = vm.Disconnect(ctx); err != nil {
		return err
	}

	// Sleep a bit to let the agent grpc server clean up
	// When we close connection to the agent, it needs sometime to cleanup
	// and restart listening on the communication( serial or vsock) port.
	// That time can be saved if we sleep a bit to wait for the agent to
	// come around and start listening again. The sleep is only done when
	// creating new vm templates and saves time for every new vm that are
	// created from template, so it worth the invest.
	time.Sleep(templateWaitForAgent)

	if err = vm.Pause(ctx); err != nil {
		return err
	}

	if err = vm.Save(); err != nil {
		return err
	}

	return nil
}

func (t *template) createFromTemplateVM(ctx context.Context, c vc.VMConfig) (*vc.VM, error) {
	config := t.config
	config.HypervisorConfig.BootToBeTemplate = false
	config.HypervisorConfig.BootFromTemplate = true
	config.HypervisorConfig.MemoryPath = t.statePath + "/memory"
	config.HypervisorConfig.DevicesStatePath = t.statePath + "/state"
	config.HypervisorConfig.SharedPath = c.HypervisorConfig.SharedPath
	config.HypervisorConfig.VMStorePath = c.HypervisorConfig.VMStorePath
	config.HypervisorConfig.RunStorePath = c.HypervisorConfig.RunStorePath

	if t.config.HypervisorType == vc.ClhHypervisor {
		config.HypervisorConfig.DevicesStatePath = t.statePath + "/" + clhStateFile
	}

	return vc.NewVM(ctx, config)
}

func (t *template) checkTemplateVM() error {
	_, err := os.Stat(t.statePath + "/memory")
	if err != nil {
		return err
	}

	stateFile := "/state"
	if t.config.HypervisorType == vc.ClhHypervisor {
		stateFile = "/" + clhStateFile
	}
	_, err = os.Stat(t.statePath + stateFile)
	return err
}

// Logger returns a logrus logger appropriate for logging template messages
func (t *template) Logger() *logrus.Entry {
	return templateLog.WithFields(logrus.Fields{
		"subsystem": "template",
	})
}

// SetLogger sets the logger for the factory template.
func SetLogger(ctx context.Context, logger logrus.FieldLogger) {
	fields := logrus.Fields{
		"source": "virtcontainers",
	}

	templateLog = logger.WithFields(fields)
}
