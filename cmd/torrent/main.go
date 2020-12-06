// Downloads torrents from the command-line.
package main

import (
	"expvar"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/anacrolix/missinggo"
	"github.com/dustin/go-humanize"
	"golang.org/x/xerrors"

	"github.com/anacrolix/log"

	"github.com/anacrolix/envpprof"
	"github.com/anacrolix/tagflag"
	"golang.org/x/time/rate"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/iplist"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/davecgh/go-spew/spew"
	"github.com/anacrolix/torrent/tracker"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"encoding/json"
)

var sLastTime time.Time
var sBytesCompleted uint64
var sBytesSpeed uint64

type BTDownloadDbus struct{
	Torr *torrent.Torrent
}

func JsonToString(resp map[string]interface{}) string {
	data, err := json.Marshal(resp)
	if err != nil {
		return ""
	}
	return *(*string)(unsafe.Pointer(&data))
}

func (btDbus *BTDownloadDbus) GetBtProgress() string {
	if btDbus.Torr == nil {
		return ""
	}
	bytesCompleted := uint64(btDbus.Torr.BytesCompleted() )
	length := uint64(btDbus.Torr.Length())

	resp := make(map[string]interface{})
	resp["progress"] = 0.0
	resp["finish"] = false
	resp["size"] = length
	resp["completed"] = bytesCompleted
	
	currentTime := time.Now()
	sub := currentTime.Sub(sLastTime)
	subSeconds := uint64(sub.Seconds())
	if subSeconds > 3 || (sBytesSpeed == 0 && subSeconds != 0) {
		sBytesSpeed = (bytesCompleted - sBytesCompleted) / subSeconds
		sBytesCompleted = bytesCompleted
		sLastTime = currentTime
	}

	resp["speed"] = sBytesSpeed
	
	mbsCompleted := float64(bytesCompleted / (1000 * 1000))
	mbsLength := float64(length / (1000 * 1000))
	if mbsLength != 0 {
		resp["progress"] = float32(mbsCompleted / mbsLength)
	} else if length != 0{
		resp["progress"] = float32(bytesCompleted) / float32(length)
	}

	if bytesCompleted == length {
		resp["finish"] = true
	}
	
	return JsonToString(resp)

}

func announceErr(args []string, parent *tagflag.Parser) error {
        var flags struct {
                tagflag.StartPos
                Tracker  string
                InfoHash torrent.InfoHash
        }
        tagflag.ParseArgs(&flags, args, tagflag.Parent(parent))
        response, err := tracker.Announce{
                TrackerUrl: flags.Tracker,
                Request: tracker.AnnounceRequest{
                        InfoHash: flags.InfoHash,
                        Port:     uint16(torrent.NewDefaultClientConfig().ListenPort),
                },
        }.Do()
        if err != nil {
                return fmt.Errorf("doing announce: %w", err)
        }
        spew.Dump(response)
        return nil
}

func torrentBar(t *torrent.Torrent, pieceStates bool) {
	go func() {
		if t.Info() == nil {
			fmt.Printf("getting info for %q\n", t.Name())
			<-t.GotInfo()
		}
		var lastLine string
		for {
			var completedPieces, partialPieces int
			psrs := t.PieceStateRuns()
			for _, r := range psrs {
				if r.Complete {
					completedPieces += r.Length
				}
				if r.Partial {
					partialPieces += r.Length
				}
			}
			line := fmt.Sprintf(
				"downloading %q: %s/%s, %d/%d pieces completed (%d partial)\n",
				t.Name(),
				humanize.Bytes(uint64(t.BytesCompleted())),
				humanize.Bytes(uint64(t.Length())),
				completedPieces,
				t.NumPieces(),
				partialPieces,
			)
			if line != lastLine {
				lastLine = line
				os.Stdout.WriteString(line)
			}
			if pieceStates {
				fmt.Println(psrs)
			}
			time.Sleep(time.Second)
		}
	}()
}

type stringAddr string

func (stringAddr) Network() string   { return "" }
func (me stringAddr) String() string { return string(me) }

var gBtDbus  *BTDownloadDbus

func resolveTestPeers(addrs []string) (ret []torrent.PeerInfo) {
	for _, ta := range flags.TestPeer {
		ret = append(ret, torrent.PeerInfo{
			Addr: stringAddr(ta),
		})
	}
	return
}

func addTorrents(client *torrent.Client) error {
	testPeers := resolveTestPeers(flags.TestPeer)
	for _, arg := range flags.Torrent {
		t, err := func() (*torrent.Torrent, error) {
			if strings.HasPrefix(arg, "magnet:") {
				fmt.Println("url: ", arg)
				var t *torrent.Torrent
				var err error
				if flags.SaveAsName == "" {
					t, err = client.AddMagnet(arg)
				} else {
					t, err = client.AddMagnetSaveAs(arg, flags.SaveAsName)
				}
				
				if err != nil {
					return nil, xerrors.Errorf("error adding magnet: %w", err)
				}
				sLastTime = time.Now()
				return t, nil
			} else if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
				response, err := http.Get(arg)
				if err != nil {
					return nil, xerrors.Errorf("Error downloading torrent file: %s", err)
				}

				metaInfo, err := metainfo.Load(response.Body)
				defer response.Body.Close()
				if err != nil {
					return nil, xerrors.Errorf("error loading torrent file %q: %s\n", arg, err)
				}
				t, err := client.AddTorrent(metaInfo)
				if err != nil {
					return nil, xerrors.Errorf("adding torrent: %w", err)
				}
				return t, nil
			} else if strings.HasPrefix(arg, "infohash:") {
				t, _ := client.AddTorrentInfoHash(metainfo.NewHashFromHex(strings.TrimPrefix(arg, "infohash:")))
				return t, nil
			} else {
				metaInfo, err := metainfo.LoadFromFile(arg)
				if err != nil {
					return nil, xerrors.Errorf("error loading torrent file %q: %s\n", arg, err)
				}
				var t *torrent.Torrent
				if flags.SaveAsName == "" {
					t, err = client.AddTorrent(metaInfo)
				} else {
					t, err = client.AddTorrentSaveAs(metaInfo, flags.SaveAsName)
				}
				if err != nil {
					return nil, xerrors.Errorf("adding torrent: %w", err)
				}
				return t, nil
			}
		}()
		if err != nil {
			return xerrors.Errorf("adding torrent for %q: %w", arg, err)
		}
		if gBtDbus != nil {
			gBtDbus.Torr = t
		}
		if flags.Progress {
			fmt.Println("progress")
			torrentBar(t, flags.PieceStates)
		}
		t.AddPeers(testPeers)
		go func() {
			<-t.GotInfo()
			t.DownloadAll()
		}()
	}
	return nil
}

var flags = struct {
	Mmap            bool          `help:"memory-map torrent data"`
	TestPeer        []string      `help:"addresses of some starting peers"`
	Seed            bool          `help:"seed after download is complete"`
	Addr            string        `help:"network listen addr"`
	UploadRate      tagflag.Bytes `help:"max piece bytes to send per second"`
	DownloadRate    tagflag.Bytes `help:"max bytes per second down from peers"`
	Debug           bool
	PackedBlocklist string
	Stats           *bool
	PublicIP        net.IP
	Progress        bool
	PieceStates     bool
	Quiet           bool `help:"discard client logging"`
	Dht             bool

	TcpPeers        bool
	UtpPeers        bool
	Webtorrent      bool
	DisableWebseeds bool

	Ipv4 bool
	Ipv6 bool
	Pex  bool

	Dbus bool
	SaveAsName		string	`help:"save download file as name"`

	tagflag.StartPos

	Torrent []string `arity:"+" help:"torrent file path or magnet uri"`
	
}{
	UploadRate:   -1,
	DownloadRate: -1,
	Progress:     true,
	Dht:          true,

	TcpPeers:   true,
	UtpPeers:   true,
	Webtorrent: true,

	Ipv4: true,
	Ipv6: true,
	Pex:  true,
}

func stdoutAndStderrAreSameFile() bool {
	fi1, _ := os.Stdout.Stat()
	fi2, _ := os.Stderr.Stat()
	return os.SameFile(fi1, fi2)
}

func statsEnabled() bool {
	if flags.Stats == nil {
		return flags.Debug
	}
	return *flags.Stats
}

func exitSignalHandlers(notify *missinggo.SynchronizedEvent) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	for {
		log.Printf("close signal received: %+v", <-c)
		notify.Set()
	}
}

func main() {
	sLastTime = time.Now()
	sBytesCompleted = 0
	if err := mainErr(); err != nil {
		log.Printf("error in main: %v", err)
		os.Exit(1)
	}
}

const intro = `
<node>
	<interface name="com.gsidv.btdownload">
		<method name="GetBtProgress">
			<arg direction="out" type="string"/>
		</method>
	</interface>` + introspect.IntrospectDataString + `</node> `

func mainErr() error {
	var flags struct {
		tagflag.StartPos
		Command string
		Args    tagflag.ExcessArgs
	}
	parser := tagflag.Parse(&flags, tagflag.ParseIntermixed(false))
	switch flags.Command {
	case "announce":
		return announceErr(flags.Args, parser)
	case "download":
		return downloadErr(flags.Args, parser)
	default:
		return fmt.Errorf("unknown command %q", flags.Command)
	}
}

func downloadErr(args []string, parent *tagflag.Parser) error {
	tagflag.ParseArgs(&flags, args, tagflag.Parent(parent))
	defer envpprof.Stop()
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DisableWebseeds = flags.DisableWebseeds
	clientConfig.DisableTCP = !flags.TcpPeers
	clientConfig.DisableUTP = !flags.UtpPeers
	clientConfig.DisableIPv4 = !flags.Ipv4
	clientConfig.DisableIPv6 = !flags.Ipv6
	clientConfig.DisableAcceptRateLimiting = true
	clientConfig.NoDHT = !flags.Dht
	clientConfig.Debug = flags.Debug
	clientConfig.Seed = flags.Seed
	clientConfig.PublicIp4 = flags.PublicIP
	clientConfig.PublicIp6 = flags.PublicIP
	clientConfig.DisablePEX = !flags.Pex
	clientConfig.DisableWebtorrent = !flags.Webtorrent

	if flags.Dbus {
		conn, err := dbus.SystemBus()
		if err != nil {
			return err
		}
		defer conn.Close()
		gBtDbus = &BTDownloadDbus{}
		fmt.Println("dbus running")
		conn.ExportAll(gBtDbus, "/com/gsidv/btdownload", "com.gsidv.btdownload")
		conn.Export(introspect.Introspectable(intro), "/com/gsidv/btdownload",
			"org.freedesktop.DBus.Introspectable")

		_, err = conn.RequestName("com.gsidv.btdownload",
			dbus.NameFlagDoNotQueue)
		if err != nil {
			return err
		}
	}

	if flags.PackedBlocklist != "" {
		blocklist, err := iplist.MMapPackedFile(flags.PackedBlocklist)
		if err != nil {
			return xerrors.Errorf("loading blocklist: %v", err)
		}
		defer blocklist.Close()
		clientConfig.IPBlocklist = blocklist
	}
	if flags.Mmap {
		clientConfig.DefaultStorage = storage.NewMMap("")
	}
	if flags.Addr != "" {
		clientConfig.SetListenAddr(flags.Addr)
	}
	if flags.UploadRate != -1 {
		clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(flags.UploadRate), 256<<10)
	}
	if flags.DownloadRate != -1 {
		clientConfig.DownloadRateLimiter = rate.NewLimiter(rate.Limit(flags.DownloadRate), 1<<20)
	}
	if flags.Quiet {
		clientConfig.Logger = log.Discard
	}

	var stop missinggo.SynchronizedEvent
	defer func() {
		stop.Set()
	}()

	client, err := torrent.NewClient(clientConfig)
	if err != nil {
		return xerrors.Errorf("creating client: %v", err)
	}
	defer client.Close()
	go exitSignalHandlers(&stop)
	go func() {
		<-stop.C()
		client.Close()
	}()

	// Write status on the root path on the default HTTP muxer. This will be bound to localhost
	// somewhere if GOPPROF is set, thanks to the envpprof import.
	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		fmt.Println("handle http")
		client.WriteStatus(w)
	})
	addTorrents(client)
	defer outputStats(client)
	if client.WaitAll() {
		log.Print("downloaded ALL the torrents")
	} else {
		return xerrors.New("y u no complete torrents?!")
	}
	if flags.Seed {
		outputStats(client)
		<-stop.C()
	}
	return nil
}

func outputStats(cl *torrent.Client) {
	if !statsEnabled() {
		return
	}
	expvar.Do(func(kv expvar.KeyValue) {
		fmt.Printf("%s: %s\n", kv.Key, kv.Value)
	})
	fmt.Println("outputStats")
	cl.WriteStatus(os.Stdout)
}
