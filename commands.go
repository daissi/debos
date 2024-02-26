package debos

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"runtime"
)

type ChrootEnterMethod int

const (
	CHROOT_METHOD_NONE   = iota // No chroot in use
	CHROOT_METHOD_NSPAWN        // use nspawn to create the chroot environment
	CHROOT_METHOD_CHROOT        // use chroot to create the chroot environment
)

type Command struct {
	Architecture string            // Architecture of the chroot, nil if same as host
	Dir          string            // Working dir to run command in
	Chroot       string            // Run in the chroot at path
	ChrootMethod ChrootEnterMethod // Method to enter the chroot

	bindMounts []string /// Items to bind mount
	extraEnv   []string // Extra environment variables to set
}

type commandWrapper struct {
	label  string
	buffer *bytes.Buffer
}

func newCommandWrapper(label string) *commandWrapper {
	b := bytes.Buffer{}
	return &commandWrapper{label, &b}
}

func (w commandWrapper) out(atEOF bool) {
	for {
		s, err := w.buffer.ReadString('\n')
		if err == nil {
			log.Printf("%s | %v", w.label, s)
		} else {
			if len(s) > 0 {
				if atEOF && err == io.EOF {
					log.Printf("%s | %v\n", w.label, s)
				} else {
					w.buffer.WriteString(s)
				}
			}
			break
		}
	}
}

func (w commandWrapper) Write(p []byte) (n int, err error) {
	n, err = w.buffer.Write(p)
	w.out(false)
	return
}

func (w *commandWrapper) flush() {
	w.out(true)
}

func NewChrootCommandForContext(context DebosContext) Command {
	c := Command{Architecture: context.Architecture, Chroot: context.Rootdir, ChrootMethod: CHROOT_METHOD_NSPAWN}

	if context.EnvironVars != nil {
		for k, v := range context.EnvironVars {
			c.AddEnv(fmt.Sprintf("%s=%s", k, v))
		}
	}

	if context.Image != "" {
		path, err := RealPath(context.Image)
		if err == nil {
			c.AddBindMount(path, "")
		} else {
			log.Printf("Failed to get realpath for %s, %v", context.Image, err)
		}
		for _, p := range context.ImagePartitions {
			path, err := RealPath(p.DevicePath)
			if err != nil {
				log.Printf("Failed to get realpath for %s, %v", p.DevicePath, err)
				continue
			}
			c.AddBindMount(path, "")
		}
		c.AddBindMount("/dev/disk", "")
	}

	return c
}

func (cmd *Command) AddEnv(env string) {
	cmd.extraEnv = append(cmd.extraEnv, env)
}

func (cmd *Command) AddEnvKey(key, value string) {
	cmd.extraEnv = append(cmd.extraEnv, fmt.Sprintf("%s=%s", key, value))
}

func (cmd *Command) AddBindMount(source, target string) {
	var mount string
	if target != "" {
		mount = fmt.Sprintf("%s:%s", source, target)
	} else {
		mount = source
	}

	cmd.bindMounts = append(cmd.bindMounts, mount)
}

func (cmd *Command) saveResolvConf() (*[sha256.Size]byte, error) {
	hostconf := "/etc/resolv.conf"
	chrootedconf := path.Join(cmd.Chroot, hostconf)
	savedconf := chrootedconf + ".debos"
	var sum [sha256.Size]byte

	if cmd.ChrootMethod == CHROOT_METHOD_NONE {
		return nil, nil
	}

	// There may not be an existing resolv.conf
	if _, err := os.Lstat(chrootedconf); !os.IsNotExist(err) {
		if err = os.Rename(chrootedconf, savedconf); err != nil {
			return nil, err
		}
	}

	/* Expect a relatively small file here */
	data, err := ioutil.ReadFile(hostconf)
	if err != nil {
		return nil, err
	}
	out := []byte("# Automatically generated by Debos\n")
	out = append(out, data...)

	sum = sha256.Sum256(out)

	err = ioutil.WriteFile(chrootedconf, out, 0644)
	if err != nil {
		return nil, err
	}

	return &sum, nil
}

func (cmd *Command) restoreResolvConf(sum *[sha256.Size]byte) error {
	hostconf := "/etc/resolv.conf"
	chrootedconf := path.Join(cmd.Chroot, hostconf)
	savedconf := chrootedconf + ".debos"

	if cmd.ChrootMethod == CHROOT_METHOD_NONE || sum == nil {
		return nil
	}

	// Remove the original copy anyway
	defer os.Remove(savedconf)

	fi, err := os.Lstat(chrootedconf)

	// resolv.conf was removed during the command call
	// Nothing to do with it -- file has been changed anyway
	if os.IsNotExist(err) {
		return nil
	}

	mode := fi.Mode()
	switch {
	case mode.IsRegular():
		// Try to calculate checksum
		data, err := ioutil.ReadFile(chrootedconf)
		if err != nil {
			return err
		}
		currentsum := sha256.Sum256(data)

		// Leave the changed resolv.conf untouched
		if bytes.Compare(currentsum[:], (*sum)[:]) == 0 {
			// Remove the generated version
			if err := os.Remove(chrootedconf); err != nil {
				return err
			}

			if _, err := os.Lstat(savedconf); !os.IsNotExist(err) {
				// Restore the original version
				if err = os.Rename(savedconf, chrootedconf); err != nil {
					return err
				}
			}
		}
	case mode&os.ModeSymlink != 0:
		// If the 'resolv.conf' is a symlink
		// Nothing to do with it -- file has been changed anyway
	default:
		// File is not regular or symlink
		// Let's get out here with verbose message
		log.Printf("Warning: /etc/resolv.conf inside the chroot is not a regular file")
	}

	return nil
}

func (cmd Command) Run(label string, cmdline ...string) error {
	q := newQemuHelper(cmd)
	q.Setup()
	defer q.Cleanup()

	var options []string
	switch cmd.ChrootMethod {
	case CHROOT_METHOD_NONE:
		options = cmdline
	case CHROOT_METHOD_CHROOT:
		options = append(options, "chroot")
		options = append(options, cmd.Chroot)
		options = append(options, cmdline...)
	case CHROOT_METHOD_NSPAWN:
		// We use own resolv.conf handling
		options = append(options, "systemd-nspawn", "-q")
		options = append(options, "--resolv-conf=off")
		options = append(options, "--timezone=off")
		options = append(options, "--register=no")
		options = append(options, "--keep-unit")
		options = append(options, "--console=pipe")
		for _, e := range cmd.extraEnv {
			options = append(options, "--setenv", e)

		}
		for _, b := range cmd.bindMounts {
			options = append(options, "--bind", b)

		}
		options = append(options, "-D", cmd.Chroot)
		options = append(options, cmdline...)
	}

	exe := exec.Command(options[0], options[1:]...)
	w := newCommandWrapper(label)

	exe.Stdin = nil
	exe.Stdout = w
	exe.Stderr = w

	defer w.flush()

	if len(cmd.extraEnv) > 0 && cmd.ChrootMethod != CHROOT_METHOD_NSPAWN {
		exe.Env = append(os.Environ(), cmd.extraEnv...)
	}

	// Disable services start/stop for commands running in chroot
	if cmd.ChrootMethod != CHROOT_METHOD_NONE {
		services := ServiceHelper{cmd.Chroot}
		services.Deny()
		defer services.Allow()
	}

	// Save the original resolv.conf and copy version from host
	resolvsum, err := cmd.saveResolvConf()
	if err != nil {
		return err
	}

	if err = exe.Run(); err != nil {
		return err
	}

	// Restore the original resolv.conf if not changed
	if err = cmd.restoreResolvConf(resolvsum); err != nil {
		return err
	}

	return nil
}

type qemuHelper struct {
	qemusrc    string
	qemutarget string
}

func newQemuHelper(c Command) qemuHelper {
	q := qemuHelper{}

	if c.Chroot == "" || c.Architecture == "" {
		return q
	}

	switch c.Architecture {
	case "armhf", "armel", "arm":
		if runtime.GOARCH != "arm64" && runtime.GOARCH != "arm" {
			q.qemusrc = "/usr/bin/qemu-arm-static"
		}
	case "arm64":
		if runtime.GOARCH != "arm64" {
			q.qemusrc = "/usr/bin/qemu-aarch64-static"
		}
	case "mips":
		q.qemusrc = "/usr/bin/qemu-mips-static"
	case "mipsel":
		if runtime.GOARCH != "mips64le" && runtime.GOARCH != "mipsle" {
			q.qemusrc = "/usr/bin/qemu-mipsel-static"
		}
	case "mips64el":
		if runtime.GOARCH != "mips64le" {
			q.qemusrc = "/usr/bin/qemu-mips64el-static"
		}
	case "riscv64":
		if runtime.GOARCH != "riscv64" {
			q.qemusrc = "/usr/bin/qemu-riscv64-static"
		}
	case "i386":
		if runtime.GOARCH != "amd64" && runtime.GOARCH != "386" {
			q.qemusrc = "/usr/bin/qemu-i386-static"
		}
	case "amd64":
		if runtime.GOARCH != "amd64" {
			q.qemusrc = "/usr/bin/qemu-x86_64-static"
		}
	default:
		log.Panicf("Don't know qemu for Architecture %s", c.Architecture)
	}

	if q.qemusrc != "" {
		q.qemutarget = path.Join(c.Chroot, q.qemusrc)
	}

	return q
}

func (q qemuHelper) Setup() error {
	if q.qemusrc == "" {
		return nil
	}
	return CopyFile(q.qemusrc, q.qemutarget, 0755)
}

func (q qemuHelper) Cleanup() {
	if q.qemusrc != "" {
		os.Remove(q.qemutarget)
	}
}
