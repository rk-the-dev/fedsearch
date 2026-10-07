// Package datagen builds a synthetic security-telemetry "world": hosts,
// users, DHCP-style IP assignments that change over time, background
// traffic, and one scripted attack chain whose ground truth is known.
//
// Everything is deterministic for a given seed and anchor time, so the
// later stages of the POC (planner, merge, enrichment) can be tested
// against exact expected answers.
package datagen

import (
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// Host is an asset that the context store (the "Reef" stand-in) knows about.
type Host struct {
	Name        string
	Owner       string
	Department  string
	Kind        string // laptop | server
	Criticality string // low | medium | high
}

// Assignment says: IP belonged to Host during [From, To).
// A zero To means "still assigned". This is the temporal fact that makes
// as-of enrichment necessary — the same IP means different hosts over time.
type Assignment struct {
	IP   string
	Host string
	From time.Time
	To   time.Time
}

func (a Assignment) covers(t time.Time) bool {
	return !t.Before(a.From) && (a.To.IsZero() || t.Before(a.To))
}

// Fixed names used by the scripted attack. Kept as constants so tests and
// later POC stages can refer to them.
const (
	AttackerIP     = "185.220.101.47"
	CompromisedHst = "lt-ankit-042"
	ReassignedHst  = "lt-meera-118"
	TargetDB       = "db-prod-01"
	TargetDBIP     = "10.30.0.10"
	VPNGateway     = "vpn-gw-01"
	ServiceAccount = "svc_backup"

	ReusedIP   = "10.20.4.17"  // ankit's laptop, then meera's after DHCP churn
	AnkitNewIP = "10.20.6.88"  // ankit's laptop after churn
	MeeraOldIP = "10.20.9.201" // meera's laptop before churn
)

// World holds the static and slowly-changing context.
type World struct {
	Start, Now  time.Time
	Hosts       []Host
	Users       []string          // user names, index-aligned with laptops
	UserLaptop  map[string]string // user -> laptop host name
	Servers     []string
	Assignments []Assignment
	byHost      map[string][]Assignment
}

// NewWorld creates hosts, users and IP assignments covering [now-days, now).
func NewWorld(r *rand.Rand, now time.Time, days, nUsers int) *World {
	w := &World{
		Start:      now.AddDate(0, 0, -days),
		Now:        now,
		UserLaptop: map[string]string{},
		byHost:     map[string][]Assignment{},
	}
	reserved := map[string]bool{ReusedIP: true, AnkitNewIP: true, MeeraOldIP: true, TargetDBIP: true}
	nextIP := func(prefix string) string {
		for {
			ip := fmt.Sprintf("%s.%d.%d", prefix, 1+r.Intn(30), 2+r.Intn(250))
			if !reserved[ip] {
				reserved[ip] = true
				return ip
			}
		}
	}
	depts := []string{"Finance", "Engineering", "Sales", "HR", "Operations", "Security"}

	// Scripted actors first.
	w.addHost(Host{CompromisedHst, "ankit.sharma", "Finance", "laptop", "medium"})
	w.addHost(Host{ReassignedHst, "meera.iyer", "Engineering", "laptop", "medium"})
	w.addHost(Host{TargetDB, "dba-team", "Operations", "server", "high"})
	w.addHost(Host{VPNGateway, "netops-team", "Operations", "server", "high"})
	w.Users = append(w.Users, "ankit.sharma", "meera.iyer")
	w.UserLaptop["ankit.sharma"] = CompromisedHst
	w.UserLaptop["meera.iyer"] = ReassignedHst
	w.Servers = append(w.Servers, TargetDB, VPNGateway)

	churn := now.AddDate(0, 0, -15) // the DHCP reassignment the attack relies on
	w.assign(ReusedIP, CompromisedHst, w.Start, churn)
	w.assign(AnkitNewIP, CompromisedHst, churn, time.Time{})
	w.assign(MeeraOldIP, ReassignedHst, w.Start, churn)
	w.assign(ReusedIP, ReassignedHst, churn, time.Time{})
	w.assign(TargetDBIP, TargetDB, w.Start, time.Time{})
	w.assign("10.30.0.2", VPNGateway, w.Start, time.Time{})
	reserved["10.30.0.2"] = true

	// Background servers.
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("srv-app-%02d", i)
		crit := []string{"low", "medium", "high"}[r.Intn(3)]
		w.addHost(Host{name, "platform-team", "Engineering", "server", crit})
		w.Servers = append(w.Servers, name)
		w.assign(nextIP("10.30"), name, w.Start, time.Time{})
	}

	// Background users and laptops; ~10% experience one DHCP change.
	for i := 0; i < nUsers; i++ {
		user := fmt.Sprintf("user%03d", i)
		laptop := fmt.Sprintf("lt-%s", user)
		w.addHost(Host{laptop, user, depts[r.Intn(len(depts))], "laptop", "low"})
		w.Users = append(w.Users, user)
		w.UserLaptop[user] = laptop
		if r.Float64() < 0.10 {
			at := w.Start.Add(time.Duration(r.Int63n(int64(now.Sub(w.Start)))))
			w.assign(nextIP("10.20"), laptop, w.Start, at)
			w.assign(nextIP("10.20"), laptop, at, time.Time{})
		} else {
			w.assign(nextIP("10.20"), laptop, w.Start, time.Time{})
		}
	}
	sort.Slice(w.Assignments, func(i, j int) bool {
		a, b := w.Assignments[i], w.Assignments[j]
		if a.IP != b.IP {
			return a.IP < b.IP
		}
		return a.From.Before(b.From)
	})
	return w
}

func (w *World) addHost(h Host) { w.Hosts = append(w.Hosts, h) }

func (w *World) assign(ip, host string, from, to time.Time) {
	a := Assignment{IP: ip, Host: host, From: from, To: to}
	w.Assignments = append(w.Assignments, a)
	w.byHost[host] = append(w.byHost[host], a)
}

// IPOf returns the IP a host held at time t.
func (w *World) IPOf(host string, t time.Time) string {
	for _, a := range w.byHost[host] {
		if a.covers(t) {
			return a.IP
		}
	}
	return ""
}

// HostOf returns the host holding ip at time t — the as-of lookup that the
// enrichment stage must reproduce. Used by tests as the oracle.
func (w *World) HostOf(ip string, t time.Time) string {
	for _, a := range w.Assignments {
		if a.IP == ip && a.covers(t) {
			return a.Host
		}
	}
	return ""
}
