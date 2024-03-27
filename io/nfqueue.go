package io

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/florianl/go-nfqueue"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

const (
	nfqueueNum              = 100
	nfqueueMaxPacketLen     = 0xFFFF
	nfqueueDefaultQueueSize = 128

	nfqueueConnMarkAccept = 1001
	nfqueueConnMarkDrop   = 1002

	nftFamily = "inet"
	nftTable  = "opengfw"
)

func generateNftRules(local, rst bool) (*nftTableSpec, error) {
	if local && rst {
		return nil, errors.New("tcp rst is not supported in local mode")
	}
	table := &nftTableSpec{
		Family: nftFamily,
		Table:  nftTable,
	}
	table.Defines = append(table.Defines, fmt.Sprintf("define ACCEPT_CTMARK=%d", nfqueueConnMarkAccept))
	table.Defines = append(table.Defines, fmt.Sprintf("define DROP_CTMARK=%d", nfqueueConnMarkDrop))
	table.Defines = append(table.Defines, fmt.Sprintf("define QUEUE_NUM=%d", nfqueueNum))
	if local {
		table.Chains = []nftChainSpec{
			{Chain: "INPUT", Header: "type filter hook input priority filter; policy accept;"},
			{Chain: "OUTPUT", Header: "type filter hook output priority filter; policy accept;"},
		}
	} else {
		table.Chains = []nftChainSpec{
			{Chain: "FORWARD", Header: "type filter hook forward priority filter; policy accept;"},
		}
	}
	for i := range table.Chains {
		c := &table.Chains[i]
		c.Rules = append(c.Rules, "ct mark $ACCEPT_CTMARK counter accept")
		if rst {
			c.Rules = append(c.Rules, "ip protocol tcp ct mark $DROP_CTMARK counter reject with tcp reset")
		}
		c.Rules = append(c.Rules, "ct mark $DROP_CTMARK counter drop")
		c.Rules = append(c.Rules, "counter queue num $QUEUE_NUM bypass")
	}
	return table, nil
}

func generateIptRules(local, rst bool) ([]iptRule, error) {
	if local && rst {
		return nil, errors.New("tcp rst is not supported in local mode")
	}
	var chains []string
	if local {
		chains = []string{"INPUT", "OUTPUT"}
	} else {
		chains = []string{"FORWARD"}
	}
	rules := make([]iptRule, 0, 4*len(chains))
	for _, chain := range chains {
		rules = append(rules, iptRule{"filter", chain, []string{"-m", "connmark", "--mark", strconv.Itoa(nfqueueConnMarkAccept), "-j", "ACCEPT"}})
		if rst {
			rules = append(rules, iptRule{"filter", chain, []string{"-p", "tcp", "-m", "connmark", "--mark", strconv.Itoa(nfqueueConnMarkDrop), "-j", "REJECT", "--reject-with", "tcp-reset"}})
		}
		rules = append(rules, iptRule{"filter", chain, []string{"-m", "connmark", "--mark", strconv.Itoa(nfqueueConnMarkDrop), "-j", "DROP"}})
		rules = append(rules, iptRule{"filter", chain, []string{"-j", "NFQUEUE", "--queue-num", strconv.Itoa(nfqueueNum), "--queue-bypass"}})
	}

	return rules, nil
}

var _ PacketIO = (*nfqueuePacketIO)(nil)

var errNotNFQueuePacket = errors.New("not an NFQueue packet")

type nfqueuePacketIO struct {
	n     *nfqueue.Nfqueue
	local bool
	rst   bool
	rSet  bool // whether the nftables/iptables rules have been set

	// iptables not nil = use iptables instead of nftables
	ipt4 *iptables.IPTables
	ipt6 *iptables.IPTables
}

type NFQueuePacketIOConfig struct {
	QueueSize   uint32
	ReadBuffer  int
	WriteBuffer int
	Local       bool
	RST         bool
}

func NewNFQueuePacketIO(config NFQueuePacketIOConfig) (PacketIO, error) {
	if config.QueueSize == 0 {
		config.QueueSize = nfqueueDefaultQueueSize
	}
	var ipt4, ipt6 *iptables.IPTables
	var err error
	if nftCheck() != nil {
		// We prefer nftables, but if it's not available, fall back to iptables
		ipt4, err = iptables.NewWithProtocol(iptables.ProtocolIPv4)
		if err != nil {
			return nil, err
		}
		ipt6, err = iptables.NewWithProtocol(iptables.ProtocolIPv6)
		if err != nil {
			return nil, err
		}
	}
	n, err := nfqueue.Open(&nfqueue.Config{
		NfQueue:      nfqueueNum,
		MaxPacketLen: nfqueueMaxPacketLen,
		MaxQueueLen:  config.QueueSize,
		Copymode:     nfqueue.NfQnlCopyPacket,
		Flags:        nfqueue.NfQaCfgFlagConntrack,
	})
	if err != nil {
		return nil, err
	}
	if config.ReadBuffer > 0 {
		err = n.Con.SetReadBuffer(config.ReadBuffer)
		if err != nil {
			_ = n.Close()
			return nil, err
		}
	}
	if config.WriteBuffer > 0 {
		err = n.Con.SetWriteBuffer(config.WriteBuffer)
		if err != nil {
			_ = n.Close()
			return nil, err
		}
	}
	return &nfqueuePacketIO{
		n:     n,
		local: config.Local,
		rst:   config.RST,
		ipt4:  ipt4,
		ipt6:  ipt6,
	}, nil
}

func (n *nfqueuePacketIO) Register(ctx context.Context, cb PacketCallback) error {
	err := n.n.RegisterWithErrorFunc(ctx,
		func(a nfqueue.Attribute) int {
			if ok, verdict := n.packetAttributeSanityCheck(a); !ok {
				if a.PacketID != nil {
					_ = n.n.SetVerdict(*a.PacketID, verdict)
				}
				return 0
			}
			p := &nfqueuePacket{
				id:       *a.PacketID,
				streamID: ctIDFromCtBytes(*a.Ct),
				data:     *a.Payload,
			}
			return okBoolToInt(cb(p, nil))
		},
		func(e error) int {
			if opErr := (*netlink.OpError)(nil); errors.As(e, &opErr) {
				if errors.Is(opErr.Err, unix.ENOBUFS) {
					// Kernel buffer temporarily full, ignore
					return 0
				}
			}
			return okBoolToInt(cb(nil, e))
		})
	if err != nil {
		return err
	}
	if !n.rSet {
		if n.ipt4 != nil {
			err = n.setupIpt(n.local, n.rst, false)
		} else {
			err = n.setupNft(n.local, n.rst, false)
		}
		if err != nil {
			return err
		}
		n.rSet = true
	}
	return nil
}

func (n *nfqueuePacketIO) packetAttributeSanityCheck(a nfqueue.Attribute) (ok bool, verdict int) {
	if a.PacketID == nil {
		// Re-inject to NFQUEUE is actually not possible in this condition
		return false, -1
	}
	if a.Payload == nil || len(*a.Payload) < 20 {
		// 20 is the minimum possible size of an IP packet
		return false, nfqueue.NfDrop
	}
	if a.Ct == nil {
		// Multicast packets may not have a conntrack, but only appear in local mode
		if n.local {
			return false, nfqueue.NfAccept
		}
		return false, nfqueue.NfDrop
	}
	return true, -1
}

func (n *nfqueuePacketIO) SetVerdict(p Packet, v Verdict, newPacket []byte) error {
	nP, ok := p.(*nfqueuePacket)
	if !ok {
		return &ErrInvalidPacket{Err: errNotNFQueuePacket}
	}
	switch v {
	case VerdictAccept:
		return n.n.SetVerdict(nP.id, nfqueue.NfAccept)
	case VerdictAcceptModify:
		return n.n.SetVerdictModPacket(nP.id, nfqueue.NfAccept, newPacket)
	case VerdictAcceptStream:
		return n.n.SetVerdictWithConnMark(nP.id, nfqueue.NfAccept, nfqueueConnMarkAccept)
	case VerdictDrop:
		return n.n.SetVerdict(nP.id, nfqueue.NfDrop)
	case VerdictDropStream:
		return n.n.SetVerdictWithConnMark(nP.id, nfqueue.NfDrop, nfqueueConnMarkDrop)
	default:
		// Invalid verdict, ignore for now
		return nil
	}
}

func (n *nfqueuePacketIO) Close() error {
	if n.rSet {
		if n.ipt4 != nil {
			_ = n.setupIpt(n.local, n.rst, true)
		} else {
			_ = n.setupNft(n.local, n.rst, true)
		}
		n.rSet = false
	}
	return n.n.Close()
}

func (n *nfqueuePacketIO) setupNft(local, rst, remove bool) error {
	rules, err := generateNftRules(local, rst)
	if err != nil {
		return err
	}
	rulesText := rules.String()
	if remove {
		err = nftDelete(nftFamily, nftTable)
	} else {
		// Delete first to make sure no leftover rules
		_ = nftDelete(nftFamily, nftTable)
		err = nftAdd(rulesText)
	}
	if err != nil {
		return err
	}
	return nil
}

func (n *nfqueuePacketIO) setupIpt(local, rst, remove bool) error {
	rules, err := generateIptRules(local, rst)
	if err != nil {
		return err
	}
	if remove {
		err = iptsBatchDeleteIfExists([]*iptables.IPTables{n.ipt4, n.ipt6}, rules)
	} else {
		err = iptsBatchAppendUnique([]*iptables.IPTables{n.ipt4, n.ipt6}, rules)
	}
	if err != nil {
		return err
	}
	return nil
}

var _ Packet = (*nfqueuePacket)(nil)

type nfqueuePacket struct {
	id       uint32
	streamID uint32
	data     []byte
}

func (p *nfqueuePacket) StreamID() uint32 {
	return p.streamID
}

func (p *nfqueuePacket) Data() []byte {
	return p.data
}

func okBoolToInt(ok bool) int {
	if ok {
		return 0
	} else {
		return 1
	}
}

func nftCheck() error {
	_, err := exec.LookPath("nft")
	if err != nil {
		return err
	}
	return nil
}

func nftAdd(input string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(input)
	return cmd.Run()
}

func nftDelete(family, table string) error {
	cmd := exec.Command("nft", "delete", "table", family, table)
	return cmd.Run()
}

type nftTableSpec struct {
	Defines       []string
	Family, Table string
	Chains        []nftChainSpec
}

func (t *nftTableSpec) String() string {
	chains := make([]string, 0, len(t.Chains))
	for _, c := range t.Chains {
		chains = append(chains, c.String())
	}

	return fmt.Sprintf(`
%s

table %s %s {
%s
}
`, strings.Join(t.Defines, "\n"), t.Family, t.Table, strings.Join(chains, ""))
}

type nftChainSpec struct {
	Chain  string
	Header string
	Rules  []string
}

func (c *nftChainSpec) String() string {
	return fmt.Sprintf(`
  chain %s {
    %s
    %s
  }
`, c.Chain, c.Header, strings.Join(c.Rules, "\n\x20\x20\x20\x20"))
}

type iptRule struct {
	Table, Chain string
	RuleSpec     []string
}

func iptsBatchAppendUnique(ipts []*iptables.IPTables, rules []iptRule) error {
	for _, r := range rules {
		for _, ipt := range ipts {
			err := ipt.AppendUnique(r.Table, r.Chain, r.RuleSpec...)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func iptsBatchDeleteIfExists(ipts []*iptables.IPTables, rules []iptRule) error {
	for _, r := range rules {
		for _, ipt := range ipts {
			err := ipt.DeleteIfExists(r.Table, r.Chain, r.RuleSpec...)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func ctIDFromCtBytes(ct []byte) uint32 {
	ctAttrs, err := netlink.UnmarshalAttributes(ct)
	if err != nil {
		return 0
	}
	for _, attr := range ctAttrs {
		if attr.Type == 12 { // CTA_ID
			return binary.BigEndian.Uint32(attr.Data)
		}
	}
	return 0
}
