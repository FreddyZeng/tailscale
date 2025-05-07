package ippool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"go4.org/netipx"
	"tailscale.com/tailcfg"
	"tailscale.com/tsconsensus"
	"tailscale.com/util/must"
)

func makeSetFromPrefix(pfx netip.Prefix) *netipx.IPSet {
	var ipsb netipx.IPSetBuilder
	ipsb.AddPrefix(pfx)
	return must.Get(ipsb.IPSet())
}

type FakeConsensus struct {
	ipp *ConsensusIPPool
}

func (c *FakeConsensus) ExecuteCommand(cmd tsconsensus.Command) (tsconsensus.CommandResult, error) {
	b, err := json.Marshal(cmd)
	if err != nil {
		return tsconsensus.CommandResult{}, err
	}
	result := c.ipp.Apply(&raft.Log{Data: b})
	return result.(tsconsensus.CommandResult), nil
}

func makePool(pfx netip.Prefix) *ConsensusIPPool {
	ipp := &ConsensusIPPool{
		IPSet: makeSetFromPrefix(pfx),
	}
	ipp.consensus = &FakeConsensus{ipp: ipp}
	return ipp
}

func TestConsensusIPForDomain(t *testing.T) {
	pfx := netip.MustParsePrefix("100.64.0.0/16")
	ipp := makePool(pfx)
	from := tailcfg.NodeID(1)

	a, err := ipp.IPForDomain(from, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !pfx.Contains(a) {
		t.Fatalf("expected %v to be in the prefix %v", a, pfx)
	}

	b, err := ipp.IPForDomain(from, "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !pfx.Contains(b) {
		t.Fatalf("expected %v to be in the prefix %v", b, pfx)
	}
	if b == a {
		t.Fatalf("same address issued twice %v, %v", a, b)
	}

	c, err := ipp.IPForDomain(from, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if c != a {
		t.Fatalf("expected %v to be remembered as the addr for example.com, but got %v", a, c)
	}
}

func TestConsensusPoolExhaustion(t *testing.T) {
	ipp := makePool(netip.MustParsePrefix("100.64.0.0/31"))
	from := tailcfg.NodeID(1)

	subdomains := []string{"a", "b", "c"}
	for i, sd := range subdomains {
		_, err := ipp.IPForDomain(from, fmt.Sprintf("%s.example.com", sd))
		if i < 2 && err != nil {
			t.Fatal(err)
		}
		expected := "ip pool exhausted"
		if i == 2 && err.Error() != expected {
			t.Fatalf("expected error to be '%s', got '%s'", expected, err.Error())
		}
	}
}

func TestConsensusPoolExpiry(t *testing.T) {
	ipp := makePool(netip.MustParsePrefix("100.64.0.0/31"))
	firstIP := netip.MustParseAddr("100.64.0.0")
	secondIP := netip.MustParseAddr("100.64.0.1")
	timeOfUse := time.Now()
	beforeTimeOfUse := timeOfUse.Add(-2 * time.Hour)
	afterTimeOfUse := timeOfUse.Add(2 * time.Hour)
	from := tailcfg.NodeID(1)

	// the pool is unused, we get an address, and it's marked as being used at timeOfUse
	aAddr, err := ipp.applyCheckoutAddr(from, "a.example.com", timeOfUse, timeOfUse)
	if err != nil {
		t.Fatal(err)
	}
	if aAddr.Compare(firstIP) != 0 {
		t.Fatalf("expected %s, got %s", firstIP, aAddr)
	}
	ww, ok := ipp.retryDomainLookup(from, firstIP, 0)
	if !ok {
		t.Fatal("expected wherewhen to be found")
	}
	d := ww.Domain
	if d != "a.example.com" {
		t.Fatalf("expected aAddr to look up to a.example.com, got: %s", d)
	}

	// the time before which we will reuse addresses is prior to timeOfUse, so no reuse
	bAddr, err := ipp.applyCheckoutAddr(from, "b.example.com", beforeTimeOfUse, timeOfUse)
	if err != nil {
		t.Fatal(err)
	}
	if bAddr.Compare(secondIP) != 0 {
		t.Fatalf("expected %s, got %s", secondIP, bAddr)
	}

	// the time before which we will reuse addresses is after timeOfUse, so reuse addresses that were marked as used at timeOfUse.
	cAddr, err := ipp.applyCheckoutAddr(from, "c.example.com", afterTimeOfUse, timeOfUse)
	if err != nil {
		t.Fatal(err)
	}
	if cAddr.Compare(firstIP) != 0 {
		t.Fatalf("expected %s, got %s", firstIP, cAddr)
	}
	ww, ok = ipp.retryDomainLookup(from, firstIP, 0)
	if !ok {
		t.Fatal("expected wherewhen to be found")
	}
	d = ww.Domain
	if d != "c.example.com" {
		t.Fatalf("expected firstIP to look up to c.example.com, got: %s", d)
	}

	// the addr remains associated with c.example.com
	cAddrAgain, err := ipp.applyCheckoutAddr(from, "c.example.com", beforeTimeOfUse, timeOfUse)
	if err != nil {
		t.Fatal(err)
	}
	if cAddrAgain.Compare(cAddr) != 0 {
		t.Fatalf("expected cAddrAgain to be cAddr, but they are different. cAddrAgain=%s cAddr=%s", cAddrAgain, cAddr)
	}
	ww, ok = ipp.retryDomainLookup(from, firstIP, 0)
	if !ok {
		t.Fatal("expected wherewhen to be found")
	}
	d = ww.Domain
	if d != "c.example.com" {
		t.Fatalf("expected firstIP to look up to c.example.com, got: %s", d)
	}
}

func TestConsensusPoolExtendExpiry(t *testing.T) {
	ipp := makePool(netip.MustParsePrefix("100.64.0.0/31"))
	firstIP := netip.MustParseAddr("100.64.0.0")
	secondIP := netip.MustParseAddr("100.64.0.1")
	timeOfUse := time.Now()
	afterTimeOfUse1 := timeOfUse.Add(-1 * time.Hour)
	afterTimeOfUse2 := timeOfUse.Add(-2 * time.Hour)
	from := tailcfg.NodeID(1)

	aAddr, err := ipp.applyCheckoutAddr(from, "a.example.com", timeOfUse, timeOfUse)
	if err != nil {
		t.Fatal(err)
	}
	if aAddr.Compare(firstIP) != 0 {
		t.Fatalf("expected %s, got %s", firstIP, aAddr)
	}

	err = ipp.applyMarkLastUsed(from, aAddr, "a.example.com", afterTimeOfUse1)
	if err != nil {
		t.Fatal(err)
	}

	bAddr, err := ipp.applyCheckoutAddr(from, "b.example.com", afterTimeOfUse2, afterTimeOfUse2)
	if err != nil {
		t.Fatal(err)
	}
	if bAddr.Compare(secondIP) != 0 {
		t.Fatalf("expected %s, got %s", firstIP, aAddr)
	}
}

func TestConsensusDomainForIP(t *testing.T) {
	ipp := makePool(netip.MustParsePrefix("100.64.0.0/16"))
	from := tailcfg.NodeID(1)
	domain := "example.com"
	now := time.Now()

	d, ok := ipp.DomainForIP(from, netip.MustParseAddr("100.64.0.1"), now)
	if d != "" {
		t.Fatalf("expected an empty string if the addr is not found but got %s", d)
	}
	if ok {
		t.Fatalf("expected domain to not be found for IP, as it has never been looked up")
	}
	a, err := ipp.IPForDomain(from, domain)
	if err != nil {
		t.Fatal(err)
	}
	d2, ok := ipp.DomainForIP(from, a, now)
	if d2 != domain {
		t.Fatalf("expected %s but got %s", domain, d2)
	}
	if !ok {
		t.Fatalf("expected domain to be found for IP that was handed out for it")
	}
}

func TestConsensusSnapshot(t *testing.T) {
	pfx := netip.MustParsePrefix("100.64.0.0/16")
	ipp := makePool(pfx)
	domain := "example.com"
	expectedAddr := netip.MustParseAddr("100.64.0.0")
	expectedFrom := expectedAddr
	expectedTo := netip.MustParseAddr("100.64.255.255")
	from := tailcfg.NodeID(1)

	// pool allocates first addr for from
	ipp.IPForDomain(from, domain)
	// take a snapshot
	fsmSnap, err := ipp.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	snap := fsmSnap.(fsmSnapshot)

	// verify snapshot state matches the state we know ipp will have
	// ipset matches ipp.IPSet
	if len(snap.IPSet.Ranges) != 1 {
		t.Fatalf("expected 1, got %d", len(snap.IPSet.Ranges))
	}
	if snap.IPSet.Ranges[0].From != expectedFrom {
		t.Fatalf("want %s, got %s", expectedFrom, snap.IPSet.Ranges[0].From)
	}
	if snap.IPSet.Ranges[0].To != expectedTo {
		t.Fatalf("want %s, got %s", expectedTo, snap.IPSet.Ranges[0].To)
	}

	// perPeerMap has one entry, for from
	if len(snap.PerPeerMap) != 1 {
		t.Fatalf("expected 1, got %d", len(snap.PerPeerMap))
	}
	ps, _ := snap.PerPeerMap[from]

	// the one peer state has allocated one address, the first in the prefix
	if len(ps.DomainToAddr) != 1 {
		t.Fatalf("expected 1, got %d", len(ps.DomainToAddr))
	}
	addr := ps.DomainToAddr[domain]
	if addr != expectedAddr {
		t.Fatalf("want %s, got %s", expectedAddr.String(), addr.String())
	}
	if len(ps.AddrToDomain) != 1 {
		t.Fatalf("expected 1, got %d", len(ps.AddrToDomain))
	}
	addrPfx, err := addr.Prefix(32)
	if err != nil {
		t.Fatal(err)
	}
	ww, _ := ps.AddrToDomain[addrPfx]
	if ww.Domain != domain {
		t.Fatalf("want %s, got %s", domain, ww.Domain)
	}
}

func TestConsensusRestore(t *testing.T) {
	pfx := netip.MustParsePrefix("100.64.0.0/16")
	ipp := makePool(pfx)
	domain := "example.com"
	expectedAddr := netip.MustParseAddr("100.64.0.0")
	from := tailcfg.NodeID(1)

	ipp.IPForDomain(from, domain)
	// take the snapshot after only 1 addr allocated
	fsmSnap, err := ipp.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	snap := fsmSnap.(fsmSnapshot)

	ipp.IPForDomain(from, "b.example.com")
	ipp.IPForDomain(from, "c.example.com")
	ipp.IPForDomain(from, "d.example.com")
	// ipp now has 4 entries in domainToAddr
	ps, _ := ipp.perPeerMap.Load(from)
	if len(ps.domainToAddr) != 4 {
		t.Fatalf("want 4, got %d", len(ps.domainToAddr))
	}

	// restore the snapshot
	bs, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	ipp.Restore(io.NopCloser(bytes.NewBuffer(bs)))

	// everything should be as it was when the snapshot was taken
	if ipp.perPeerMap.Len() != 1 {
		t.Fatalf("want 1, got %d", ipp.perPeerMap.Len())
	}
	psAfter, _ := ipp.perPeerMap.Load(from)
	if len(psAfter.domainToAddr) != 1 {
		t.Fatalf("want 1, got %d", len(psAfter.domainToAddr))
	}
	if psAfter.domainToAddr[domain] != expectedAddr {
		t.Fatalf("want %s, got %s", expectedAddr, psAfter.domainToAddr[domain])
	}
	ww, _ := psAfter.addrToDomain.Lookup(expectedAddr)
	if ww.Domain != domain {
		t.Fatalf("want %s, got %s", domain, ww.Domain)
	}
}
