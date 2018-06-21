// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

//go:generate bash -c "echo -en '// AUTOGENERATED FILE\n\n' > linux_generated.go"
//go:generate bash -c "echo -en 'package build\n\n' >> linux_generated.go"
//go:generate bash -c "echo -en 'const createImageScript = `#!/bin/bash\n' >> linux_generated.go"
//go:generate bash -c "cat ../../tools/create-gce-image.sh | grep -v '#' >> linux_generated.go"
//go:generate bash -c "echo -en '`\n\n' >> linux_generated.go"

package build

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/google/syzkaller/pkg/osutil"
)

func Build(dir, compiler string, config []byte) error {
	configFile := filepath.Join(dir, ".config")
	if err := osutil.WriteFile(configFile, config); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}
	if err := osutil.SandboxChown(configFile); err != nil {
		return err
	}
	// One would expect olddefconfig here, but olddefconfig is not present in v3.6 and below.
	// oldconfig is the same as olddefconfig if stdin is not set.
	// Note: passing in compiler is important since 4.17 (at the very least it's noted in the config).
	cmd := osutil.Command("make", "oldconfig", "CC="+compiler)
	if err := osutil.Sandbox(cmd, true, true); err != nil {
		return err
	}
	cmd.Dir = dir
	if _, err := osutil.Run(10*time.Minute, cmd); err != nil {
		return err
	}
	// We build only bzImage as we currently don't use modules.
	cpu := strconv.Itoa(runtime.NumCPU())
	cmd = osutil.Command("make", "bzImage", "-j", cpu, "CC="+compiler)
	if err := osutil.Sandbox(cmd, true, true); err != nil {
		return err
	}
	cmd.Dir = dir
	// Build of a large kernel can take a while on a 1 CPU VM.
	if _, err := osutil.Run(3*time.Hour, cmd); err != nil {
		return extractRootCause(err)
	}
	return nil
}

func Clean(dir string) error {
	cpu := strconv.Itoa(runtime.NumCPU())
	cmd := osutil.Command("make", "distclean", "-j", cpu)
	if err := osutil.Sandbox(cmd, true, true); err != nil {
		return err
	}
	cmd.Dir = dir
	_, err := osutil.Run(10*time.Minute, cmd)
	return err
}

// CreateImage creates a disk image that is suitable for syzkaller.
// Kernel is taken from kernelDir, userspace system is taken from userspaceDir.
// If cmdlineFile is not empty, contents of the file are appended to the kernel command line.
// If sysctlFile is not empty, contents of the file are appended to the image /etc/sysctl.conf.
// Produces image and root ssh key in the specified files.
func CreateImage(targetOS, targetArch, vmType, kernelDir, userspaceDir, cmdlineFile, sysctlFile,
	image, sshkey string) error {
	if targetOS != "linux" || targetArch != "amd64" {
		return fmt.Errorf("only linux/amd64 is supported")
	}
	if vmType != "qemu" && vmType != "gce" {
		return fmt.Errorf("images can be built only for qemu/gce machines")
	}
	tempDir, err := ioutil.TempDir("", "syz-build")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	scriptFile := filepath.Join(tempDir, "create.sh")
	if err := osutil.WriteExecFile(scriptFile, []byte(createImageScript)); err != nil {
		return fmt.Errorf("failed to write script file: %v", err)
	}
	bzImage := filepath.Join(kernelDir, filepath.FromSlash("arch/x86/boot/bzImage"))
	cmd := osutil.Command(scriptFile, userspaceDir, bzImage)
	cmd.Dir = tempDir
	cmd.Env = append([]string{}, os.Environ()...)
	cmd.Env = append(cmd.Env,
		"SYZ_VM_TYPE="+vmType,
		"SYZ_CMDLINE_FILE="+osutil.Abs(cmdlineFile),
		"SYZ_SYSCTL_FILE="+osutil.Abs(sysctlFile),
	)
	if _, err = osutil.Run(time.Hour, cmd); err != nil {
		return fmt.Errorf("image build failed: %v", err)
	}
	// Note: we use CopyFile instead of Rename because src and dst can be on different filesystems.
	if err := osutil.CopyFile(filepath.Join(tempDir, "disk.raw"), image); err != nil {
		return err
	}
	if err := osutil.CopyFile(filepath.Join(tempDir, "key"), sshkey); err != nil {
		return err
	}
	if err := os.Chmod(sshkey, 0600); err != nil {
		return err
	}
	return nil
}

func extractRootCause(err error) error {
	verr, ok := err.(*osutil.VerboseError)
	if !ok {
		return err
	}
	var cause []byte
	for _, line := range bytes.Split(verr.Output, []byte{'\n'}) {
		for _, pattern := range buildFailureCauses {
			if pattern.weak && cause != nil {
				continue
			}
			if bytes.Contains(line, pattern.pattern) {
				cause = line
				break
			}
		}
	}
	if cause != nil {
		verr.Title = string(cause)
	}
	return verr
}

type buildFailureCause struct {
	pattern []byte
	weak    bool
}

var buildFailureCauses = [...]buildFailureCause{
	{pattern: []byte(": error: ")},
	{pattern: []byte(": fatal error: ")},
	{pattern: []byte(": undefined reference to")},
	{weak: true, pattern: []byte(": final link failed: ")},
	{weak: true, pattern: []byte("collect2: error: ")},
}
