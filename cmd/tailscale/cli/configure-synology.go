// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/peterbourgon/ff/v3/ffcli"
	"tailscale.com/hostinfo"
	"tailscale.com/version/distro"
)

// configureHostCmd is the "tailscale configure-host" command which was once
// used to configure Synology devices, but is now a compatibility alias to
// "tailscale configure synology".
var configureHostCmd = &ffcli.Command{
	Name:      "configure-host",
	Exec:      runConfigureSynology,
	ShortHelp: synologyConfigureCmd.ShortHelp,
	LongHelp:  synologyConfigureCmd.LongHelp,
	FlagSet: (func() *flag.FlagSet {
		fs := newFlagSet("configure-host")
		return fs
	})(),
}

var synologyConfigureCmd = &ffcli.Command{
	Name:      "synology",
	Exec:      runConfigureSynology,
	ShortHelp: "Configure Synology to enable more Tailscale features",
	LongHelp: strings.TrimSpace(`
The 'configure-host' command is intended to run at boot as root
to create the /dev/net/tun device and give the tailscaled binary
permission to use it.

See: https://tailscale.com/kb/1152/synology-outbound/
`),
	FlagSet: (func() *flag.FlagSet {
		fs := newFlagSet("synology")
		return fs
	})(),
}

func runConfigureSynology(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("unknown arguments")
	}
	if runtime.GOOS != "linux" || distro.Get() != distro.Synology {
		return errors.New("only implemented on Synology")
	}
	if uid := os.Getuid(); uid != 0 {
		return fmt.Errorf("must be run as root, not %q (%v)", os.Getenv("USER"), uid)
	}
	hi := hostinfo.New()
	isDSM6 := strings.HasPrefix(hi.DistroVersion, "6.")
	isDSM7 := strings.HasPrefix(hi.DistroVersion, "7.")
	if !isDSM6 && !isDSM7 {
		return fmt.Errorf("unsupported DSM version %q", hi.DistroVersion)
	}
	if _, err := os.Stat("/dev/net/tun"); os.IsNotExist(err) {
		if err := os.MkdirAll("/dev/net", 0755); err != nil {
			return fmt.Errorf("creating /dev/net: %v", err)
		}
		if out, err := exec.Command("/bin/mknod", "/dev/net/tun", "c", "10", "200").CombinedOutput(); err != nil {
			return fmt.Errorf("creating /dev/net/tun: %v, %s", err, out)
		}
	}
	if err := os.Chmod("/dev/net", 0755); err != nil {
		return err
	}
	if err := os.Chmod("/dev/net/tun", 0666); err != nil {
		return err
	}
	if isDSM6 {
		printf("/dev/net/tun exists and has permissions 0666. Skipping setcap on DSM6.\n")
		return nil
	}

	const daemonBin = "/var/packages/Tailscale/target/bin/tailscaled"
	if _, err := os.Stat(daemonBin); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("tailscaled binary not found at %s. Is the Tailscale *.spk package installed?", daemonBin)
		}
		return err
	}
	if out, err := exec.Command("/bin/setcap", "cap_net_admin,cap_net_raw+eip", daemonBin).CombinedOutput(); err != nil {
		return fmt.Errorf("setcap: %v, %s", err, out)
	}
	printf("Done. To restart Tailscale to use the new permissions, run:\n\n  sudo synosystemctl restart pkgctl-Tailscale.service\n\n")
	return nil
}
