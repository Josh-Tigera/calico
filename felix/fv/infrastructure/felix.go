// Copyright (c) 2017-2023 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package infrastructure

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/felix/bpf/jump"
	"github.com/projectcalico/calico/felix/bpf/polprog"

	"github.com/projectcalico/calico/felix/fv/containers"
	"github.com/projectcalico/calico/felix/fv/tcpdump"
	"github.com/projectcalico/calico/felix/fv/utils"
)

// FIXME: isolate individual Felix instances in their own cgroups.  Unfortunately, this doesn't work on systems that are using cgroupv1
// see https://elixir.bootlin.com/linux/v5.3.11/source/include/linux/cgroup-defs.h#L788 for explanation.
const CreateCgroupV2 = false

type Felix struct {
	*containers.Container

	// ExpectedIPIPTunnelAddr contains the IP that the infrastructure expects to
	// get assigned to the IPIP tunnel.  Filled in by SetExpectedIPIPTunnelAddr().
	ExpectedIPIPTunnelAddr string
	// ExpectedVXLANTunnelAddr contains the IP that the infrastructure expects to
	// get assigned to the IPv4 VXLAN tunnel.  Filled in by SetExpectedVXLANTunnelAddr().
	ExpectedVXLANTunnelAddr string
	// ExpectedVXLANV6TunnelAddr contains the IP that the infrastructure expects to
	// get assigned to the IPv6 VXLAN tunnel.  Filled in by SetExpectedVXLANV6TunnelAddr().
	ExpectedVXLANV6TunnelAddr string
	// ExpectedWireguardTunnelAddr contains the IPv4 address that the infrastructure expects to
	// get assigned to the IPv4 Wireguard tunnel.  Filled in by SetExpectedWireguardTunnelAddr().
	ExpectedWireguardTunnelAddr string
	// ExpectedWireguardV6TunnelAddr contains the IPv6 address that the infrastructure expects to
	// get assigned to the IPv6 Wireguard tunnel.  Filled in by SetExpectedWireguardV6TunnelAddr().
	ExpectedWireguardV6TunnelAddr string

	// IP of the Typha that this Felix is using (if any).
	TyphaIP string

	// If set, acts like an external IP of a node. Filled in by SetExternalIP().
	ExternalIP string

	startupDelayed bool
	restartDelayed bool
	Workloads      []workload

	TopologyOptions TopologyOptions
}

type workload interface {
	Runs() bool
	GetIP() string
	GetInterfaceName() string
	GetSpoofInterfaceName() string
}

func (f *Felix) GetFelixPID() int {
	if f.startupDelayed {
		log.Panic("GetFelixPID() called but startup is delayed")
	}
	if f.restartDelayed {
		log.Panic("GetFelixPID() called but restart is delayed")
	}
	return f.GetSinglePID("calico-felix")
}

func (f *Felix) GetFelixPIDs() []int {
	if f.startupDelayed {
		log.Panic("GetFelixPIDs() called but startup is delayed")
	}
	if f.restartDelayed {
		log.Panic("GetFelixPIDs() called but restart is delayed")
	}
	return f.GetPIDs("calico-felix")
}

func (f *Felix) TriggerDelayedStart() {
	if !f.startupDelayed {
		log.Panic("TriggerDelayedStart() called but startup wasn't delayed")
	}
	f.Exec("touch", "/start-trigger")
	f.startupDelayed = false
}

func RunFelix(infra DatastoreInfra, id int, options TopologyOptions) *Felix {
	log.Info("Starting felix")
	ipv6Enabled := fmt.Sprint(options.EnableIPv6)
	bpfEnableIPv6 := fmt.Sprint(options.BPFEnableIPv6)

	args := infra.GetDockerArgs()
	args = append(args, "--privileged")

	// Collect the environment variables for starting this particular container.  Note: we
	// are called concurrently with other instances of RunFelix so it's important to only
	// read from options.*.
	envVars := map[string]string{
		// Enable core dumps.
		"GOTRACEBACK": "crash",
		"GORACE":      "history_size=2",
		// Tell the wrapper to set the core file name pattern so we can find the dump.
		"SET_CORE_PATTERN": "true",

		"FELIX_LOGSEVERITYSCREEN":        options.FelixLogSeverity,
		"FELIX_LogDebugFilenameRegex":    options.FelixDebugFilenameRegex,
		"FELIX_PROMETHEUSMETRICSENABLED": "true",
		"FELIX_BPFLOGLEVEL":              "debug",
		"FELIX_USAGEREPORTINGENABLED":    "false",
		"FELIX_IPV6SUPPORT":              ipv6Enabled,
		"FELIX_BPFIPV6SUPPORT":           bpfEnableIPv6,
		// Disable log dropping, because it can cause flakes in tests that look for particular logs.
		"FELIX_DEBUGDISABLELOGDROPPING": "true",
	}
	// Collect the volumes for this container.
	wd, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred(), "failed to get working directory")

	arch := utils.GetSysArch()

	fvBin := os.Getenv("FV_BINARY")
	if fvBin == "" {
		fvBin = fmt.Sprintf("bin/calico-felix-%s", arch)
	}
	volumes := map[string]string{
		path.Join(wd, "..", "bin"):        "/usr/local/bin",
		path.Join(wd, "..", fvBin):        "/usr/local/bin/calico-felix",
		path.Join(wd, "..", "bin", "bpf"): "/usr/lib/calico/bpf/",
		"/lib/modules":                    "/lib/modules",
		"/tmp":                            "/tmp",
	}

	containerName := containers.UniqueName(fmt.Sprintf("felix-%d", id))

	if os.Getenv("FELIX_FV_ENABLE_BPF") == "true" {
		if !options.TestManagesBPF {
			log.Info("FELIX_FV_ENABLE_BPF=true, enabling BPF with env var")
			envVars["FELIX_BPFENABLED"] = "true"
		} else {
			log.Info("FELIX_FV_ENABLE_BPF=true but test manages BPF state itself, not using env var")
		}

		if CreateCgroupV2 {
			envVars["FELIX_DEBUGBPFCGROUPV2"] = containerName
		}
	}

	if options.DelayFelixStart {
		envVars["DELAY_FELIX_START"] = "true"
	}

	for k, v := range options.ExtraEnvVars {
		envVars[k] = v
	}

	for k, v := range envVars {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	// Add in the volumes.
	for k, v := range options.ExtraVolumes {
		volumes[k] = v
	}
	for k, v := range volumes {
		args = append(args, "-v", fmt.Sprintf("%s:%s", k, v))
	}

	args = append(args,
		utils.Config.FelixImage,
	)

	felixOpts := containers.RunOpts{
		AutoRemove: true,
	}
	if options.FelixStopGraceful {
		// Leave StopSignal defaulting to SIGTERM, and allow 10 seconds for Felix
		// to handle that gracefully.
		felixOpts.StopTimeoutSecs = 10
	} else {
		// Use SIGKILL to stop Felix immediately.
		felixOpts.StopSignal = "SIGKILL"
	}
	c := containers.RunWithFixedName(containerName, felixOpts, args...)

	if options.EnableIPv6 {
		c.Exec("sysctl", "-w", "net.ipv6.conf.all.disable_ipv6=0")
		c.Exec("sysctl", "-w", "net.ipv6.conf.default.disable_ipv6=0")
		c.Exec("sysctl", "-w", "net.ipv6.conf.lo.disable_ipv6=0")
		c.Exec("sysctl", "-w", "net.ipv6.conf.all.forwarding=1")
	} else {
		c.Exec("sysctl", "-w", "net.ipv6.conf.all.disable_ipv6=1")
		c.Exec("sysctl", "-w", "net.ipv6.conf.default.disable_ipv6=1")
		c.Exec("sysctl", "-w", "net.ipv6.conf.lo.disable_ipv6=1")
		c.Exec("sysctl", "-w", "net.ipv6.conf.all.forwarding=0")
	}

	// Configure our model host to drop forwarded traffic by default.  Modern
	// Kubernetes/Docker hosts now have this setting, and the consequence is that
	// whenever Calico policy intends to allow a packet, it must explicitly ACCEPT
	// that packet, not just allow it to pass through cali-FORWARD and assume it will
	// be accepted by the rest of the chain.  Establishing that setting in this FV
	// allows us to test that.
	c.Exec("iptables",
		"-w", "10", // Retry this for 10 seconds, e.g. if something else is holding the lock
		"-W", "100000", // How often to probe the lock in microsecs.
		"-P", "FORWARD", "DROP")

	return &Felix{
		Container:       c,
		startupDelayed:  options.DelayFelixStart,
		TopologyOptions: options,
	}
}

func (f *Felix) Stop() {
	if CreateCgroupV2 {
		_ = f.ExecMayFail("rmdir", path.Join("/run/calico/cgroup/", f.Name))
	}
	f.Container.Stop()
}

func (f *Felix) Restart() {
	oldPID := f.GetFelixPID()
	f.Exec("kill", "-HUP", fmt.Sprint(oldPID))
	Eventually(f.GetFelixPID, "10s", "100ms").ShouldNot(Equal(oldPID))
}

func (f *Felix) RestartWithDelayedStartup() func() {
	if f.restartDelayed {
		log.Panic("RestartWithDelayedStartup() called but restart was delayed already")
	}
	oldPID := f.GetFelixPID()
	f.restartDelayed = true
	f.Exec("touch", "/delay-felix-restart")
	f.Exec("kill", "-HUP", fmt.Sprint(oldPID))
	triggerChan := make(chan struct{})

	go func() {
		defer GinkgoRecover()
		select {
		case <-time.After(time.Second * 30):
			log.Panic("Restart with delayed startup timed out after 30s")
		case <-triggerChan:
			return
		}
	}()

	return func() {
		close(triggerChan)
		f.Exec("rm", "/delay-felix-restart")
		f.restartDelayed = false
		Eventually(f.GetFelixPID, "10s", "100ms").ShouldNot(Equal(oldPID))
	}
}

func (f *Felix) SetEnv(env map[string]string) {
	fn := "extra-env.sh"

	file, err := os.OpenFile("./"+fn, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
	Expect(err).NotTo(HaveOccurred())

	fw := bufio.NewWriter(file)

	for k, v := range env {
		fmt.Fprintf(fw, "export %s=%v\n", k, v)
	}

	fw.Flush()
	file.Close()

	err = f.CopyFileIntoContainer("./"+fn, "/"+fn)
	Expect(err).NotTo(HaveOccurred())
}

// AttachTCPDump returns tcpdump attached to the container
func (f *Felix) AttachTCPDump(iface string) *tcpdump.TCPDump {
	return tcpdump.Attach(f.Container.Name, "", iface)
}

func (f *Felix) ProgramIptablesDNAT(serviceIP, targetIP, chain string, ipv6 bool) {
	if !ipv6 {
		f.Exec(
			"iptables",
			"-w", "10", // Retry this for 10 seconds, e.g. if something else is holding the lock
			"-W", "100000", // How often to probe the lock in microsecs.
			"-t", "nat", "-A", chain,
			"--destination", serviceIP,
			"-j", "DNAT", "--to-destination", targetIP,
		)
	} else {
		f.Exec(
			"ip6tables",
			"-w", "10", // Retry this for 10 seconds, e.g. if something else is holding the lock
			"-W", "100000", // How often to probe the lock in microsecs.
			"-t", "nat", "-A", chain,
			"-d", serviceIP,
			"-j", "DNAT", "--to-destination", targetIP,
		)
	}
}

type BPFIfState struct {
	IfIndex         int
	Workload        bool
	V4Ready         bool
	V6Ready         bool
	IngressPolicyV4 int
	EgressPolicyV4  int
	IngressPolicyV6 int
	EgressPolicyV6  int
}

var bpfIfStateRegexp = regexp.MustCompile(`.*([0-9]+) : \{flags: (.*) name: (.*)}`)

func (f *Felix) BPFIfState(family int) map[string]BPFIfState {
	out, err := f.ExecOutput("calico-bpf", "ifstate", "dump")
	Expect(err).NotTo(HaveOccurred())

	states := make(map[string]BPFIfState)

	lines := strings.Split(out, "\n")
	for _, line := range lines {
		match := bpfIfStateRegexp.FindStringSubmatch(line)
		if len(match) == 0 {
			continue
		}

		name := match[3]
		flags := match[2]
		ifIndex, _ := strconv.Atoi(match[1])

		inPolV4 := -1
		outPolV4 := -1
		inPolV6 := -1
		outPolV6 := -1
		if family == 4 {
			r := regexp.MustCompile(`IngressPolicyV4: (\d+)`)
			m := r.FindStringSubmatch(line)
			inPolV4, _ = strconv.Atoi(m[1])
			r = regexp.MustCompile(`EgressPolicyV4: (\d+)`)
			m = r.FindStringSubmatch(line)
			outPolV4, _ = strconv.Atoi(m[1])
		} else {
			r := regexp.MustCompile(`IngressPolicyV6: (\d+)`)
			m := r.FindStringSubmatch(line)
			inPolV6, _ = strconv.Atoi(m[1])
			r = regexp.MustCompile(`EgressPolicyV6: (\d+)`)
			m = r.FindStringSubmatch(line)
			outPolV6, _ = strconv.Atoi(m[1])
		}

		state := BPFIfState{
			IfIndex:         ifIndex,
			Workload:        strings.Contains(flags, "workload"),
			V4Ready:         strings.Contains(flags, "v4Ready"),
			V6Ready:         strings.Contains(flags, "v6Ready"),
			IngressPolicyV4: inPolV4,
			EgressPolicyV4:  outPolV4,
			IngressPolicyV6: inPolV6,
			EgressPolicyV6:  outPolV6,
		}

		states[name] = state
	}

	return states
}

func (f *Felix) BPFNumContiguousPolProgramsFn(iface string, ingressOrEgress string, family int) func() int {
	return func() int {
		cont, _ := f.BPFNumPolProgramsByName(iface, ingressOrEgress, family)
		return cont
	}
}

func (f *Felix) BPFNumPolProgramsByName(iface string, ingressOrEgress string, family int) (contiguous, total int) {
	entryPointIdx := f.BPFPolEntryPointIdx(iface, ingressOrEgress, family)
	return f.BPFNumPolProgramsByEntryPoint(entryPointIdx)
}

func (f *Felix) BPFPolEntryPointIdx(iface string, ingressOrEgress string, family int) int {
	ifState := f.BPFIfState(family)[iface]
	var entryPointIdx int
	if ingressOrEgress == "ingress" {
		entryPointIdx = ifState.IngressPolicyV4
		if family == 6 {
			entryPointIdx = ifState.IngressPolicyV6
		}
	} else {
		entryPointIdx = ifState.EgressPolicyV4
		if family == 6 {
			entryPointIdx = ifState.EgressPolicyV6
		}
	}
	return entryPointIdx
}

func (f *Felix) BPFNumPolProgramsTotalByEntryPointFn(entryPointIdx int) func() (total int) {
	return func() (total int) {
		_, total = f.BPFNumPolProgramsByEntryPoint(entryPointIdx)
		return
	}
}

func (f *Felix) BPFNumPolProgramsByEntryPoint(entryPointIdx int) (contiguous, total int) {
	gapSeen := false
	for i := 0; i < jump.MaxSubPrograms; i++ {
		k := polprog.SubProgramJumpIdx(entryPointIdx, i, jump.TCMaxEntryPoints)
		out, err := f.ExecOutput(
			"bpftool", "map", "lookup",
			"pinned", "/sys/fs/bpf/tc/globals/cali_jump3",
			"key",
			fmt.Sprintf("%d", k&0xff),
			fmt.Sprintf("%d", (k>>8)&0xff),
			fmt.Sprintf("%d", (k>>16)&0xff),
			fmt.Sprintf("%d", (k>>24)&0xff),
		)
		if err != nil {
			gapSeen = true
		}
		if strings.Contains(out, "value:") {
			total++
			if !gapSeen {
				contiguous++
			}
		} else {
			gapSeen = true
		}
	}
	return
}

func (f *Felix) IPTablesChains(table string) map[string][]string {
	out := map[string][]string{}
	raw, err := f.ExecOutput("iptables-save", "-t", table)
	Expect(err).NotTo(HaveOccurred())
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			// Line is a comment, ignore.
			continue
		}
		if strings.HasPrefix(line, ":") {
			// A chain declaration line, for example:
			// :cali-INPUT - [0:0]
			chainName := strings.SplitN(line[1:], " ", 2)[0]
			out[chainName] = []string{}
			continue
		}
		if strings.HasPrefix(line, "-A") {
			// "-A" means "append rule to chain".  For example:
			// -A PREROUTING -m addrtype --dst-type LOCAL -j DOCKER
			chainName := strings.SplitN(line[3:], " ", 2)[0]
			out[chainName] = append(out[chainName], line)
			continue
		}
	}
	return out
}
