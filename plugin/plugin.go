package plugin

import (
	"context"
	"fmt"
	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	redisCon "github.com/gomodule/redigo/redis"
	"github.com/miekg/dns"
	redis "github.com/rverst/coredns-redis"
	"github.com/rverst/coredns-redis/record"
	"sync"
	"time"
)

const name = "redis"

var log = clog.NewWithPlugin("redis")

type Plugin struct {
	Redis *redis.Redis
	Next  plugin.Handler

	loadZoneTicker *time.Ticker
	zones          []string
	lock           sync.Mutex
}

func (p *Plugin) Name() string {
	return name
}

func (p *Plugin) Ready() bool {
	ok, err := p.Redis.Ping()
	if err != nil {
		log.Error(err)
	}
	return ok
}

func (p *Plugin) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{Req: r, W: w}
	qName := state.Name()
	qType := state.QType()

	if qName == "" || qType == dns.TypeNone {
		return plugin.NextOrFailure(qName, p.Next, ctx, w, r)
	}

	conn := p.Redis.Pool.Get()
	defer conn.Close()

	//zones, err, connOk := p.Redis.LoadZoneNamesC(qName, conn)
	//if err != nil {
	//	log.Error(err)
	//	if !connOk {
	//		return dns.RcodeServerFailure, err
	//	}
	//	return plugin.NextOrFailure(qName, p.Next, ctx, w, r)
	//}
	zoneName := plugin.Zones(p.zones).Matches(qName)
	if zoneName == "" {
		log.Debugf("zone not found: %s", qName)
		return plugin.NextOrFailure(qName, p.Next, ctx, w, r)
	}

	zone := p.Redis.LoadZoneC(zoneName, false, conn)
	if zone == nil {
		log.Errorf("unable to load zone: %s", zoneName)
		return p.Redis.ErrorResponse(state, zoneName, dns.RcodeServerFailure, nil)
	}

	if qType == dns.TypeAXFR {
		log.Debug("zone transfer request (Handler)")
		return p.handleZoneTransfer(zone, p.zones, w, r, conn)
	}

	location := p.Redis.FindLocation(qName, zone)
	if location == "" {
		log.Debugf("location %s not found for zone: %s", qName, zone)
		return p.Redis.ErrorResponse(state, zoneName, dns.RcodeNameError, nil)
	}

	answers := make([]dns.RR, 0, 0)
	extras := make([]dns.RR, 0, 10)
	zoneRecords := p.Redis.LoadZoneRecordsC(location, zone, conn)
	zoneRecords.MakeFqdn(zone.Name)

	switch qType {
	case dns.TypeSOA:
		answers, extras = p.Redis.SOA(zone, zoneRecords)
	case dns.TypeA:
		answers, extras = p.Redis.A(qName, zone, zoneRecords)
	case dns.TypeAAAA:
		answers, extras = p.Redis.AAAA(qName, zone, zoneRecords)
	case dns.TypeCNAME:
		answers, extras = p.Redis.CNAME(qName, zone, zoneRecords)
	case dns.TypeTXT:
		answers, extras = p.Redis.TXT(qName, zone, zoneRecords)
	case dns.TypeNS:
		answers, extras = p.Redis.NS(qName, zone, zoneRecords, p.zones, conn)
	case dns.TypeMX:
		answers, extras = p.Redis.MX(qName, zone, zoneRecords, p.zones, conn)
	case dns.TypeSRV:
		answers, extras = p.Redis.SRV(qName, zone, zoneRecords, p.zones, conn)
	case dns.TypePTR:
		answers, extras = p.Redis.PTR(qName, zone, zoneRecords, p.zones, conn)
	case dns.TypeCAA:
		answers, extras = p.Redis.CAA(qName, zone, zoneRecords)

	default:
		return p.Redis.ErrorResponse(state, zoneName, dns.RcodeNotImplemented, nil)
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative, m.RecursionAvailable, m.Compress = true, false, true
	m.Answer = append(m.Answer, answers...)
	m.Extra = append(m.Extra, extras...)
	state.SizeAndDo(m)
	m = state.Scrub(m)
	_ = w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}

func (p *Plugin) handleZoneTransfer(zone *record.Zone, zones []string, w dns.ResponseWriter, r *dns.Msg, conn redisCon.Conn) (int, error) {
	//todo: check and test zone transfer, implement ip-range check
	records := p.Redis.AXFR(zone, zones, conn)
	ch := make(chan *dns.Envelope)
	tr := new(dns.Transfer)
	tr.TsigSecret = nil
	go func(ch chan *dns.Envelope) {
		j, l := 0, 0

		for i, r := range records {
			l += dns.Len(r)
			if l > redis.MaxTransferLength {
				ch <- &dns.Envelope{RR: records[j:i]}
				l = 0
				j = i
			}
		}
		if j < len(records) {
			ch <- &dns.Envelope{RR: records[j:]}
		}
		close(ch)
	}(ch)

	err := tr.Out(w, r, ch)
	if err != nil {
		fmt.Println(err)
	}
	w.Hijack()
	return dns.RcodeSuccess, nil
}

func (p *Plugin) startZoneNameCache() {

	z, err := p.Redis.LoadAllZoneNames()
	if err != nil {
		log.Fatal("unable to load zones to cache", err)
	}
	p.lock.Lock()
	p.zones = z
	p.lock.Unlock()
	log.Info("zone name cache loaded")
	go func() {
		select {
		case <- p.loadZoneTicker.C:
			z, err := p.Redis.LoadAllZoneNames()
			if err != nil {
				log.Error("unable to load zones to cache", err)
			}
			p.lock.Lock()
			p.zones = z
			p.lock.Unlock()
			log.Info("zone name cache refreshed")
		}
	}()

}
