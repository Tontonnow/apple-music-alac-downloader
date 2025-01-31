package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/abema/go-mp4"
	"github.com/gin-gonic/gin"
	"github.com/grafov/m3u8"
	"gopkg.in/yaml.v3"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultId   = "0"
	prefetchKey = "skd://itunes.apple.com/P000000000/s1/e1"
)

var (
	forbiddenNames = regexp.MustCompile(`[/\\<>:"|?*]`)
)

const (
	userAgent = `Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/51.0.2704.103 Safari/537.36`
)

type HttpDownloader struct {
	url           string
	filename      string
	contentLength int
	acceptRanges  bool // 是否支持断点续传
	numThreads    int  // 同时下载线程数
	err           error
}

func (h *HttpDownloader) check(e error) {
	if e != nil {
		log.Println(e)
		h.err = e
	}
}

func NewDownload(url string, numThreads int) *HttpDownloader {
	var urlSplits []string = strings.Split(url, "/")
	var filename string = urlSplits[len(urlSplits)-1]
	httpDownload := new(HttpDownloader)
	res, err := http.Head(url)
	httpDownload.check(err)

	httpDownload.url = url
	httpDownload.contentLength = int(res.ContentLength)
	httpDownload.numThreads = numThreads
	httpDownload.filename = filename

	if len(res.Header["Accept-Ranges"]) != 0 && res.Header["Accept-Ranges"][0] == "bytes" {
		httpDownload.acceptRanges = true
	} else {
		httpDownload.acceptRanges = false
	}

	return httpDownload
}

func (h *HttpDownloader) Download() []byte {
	var data []byte

	if h.acceptRanges == false {
		fmt.Println("该文件不支持多线程下载，单线程下载中：")
		resp, err := http.Get(h.url)
		h.check(err)
		data = h.save2memory(resp)
	} else {
		var wg sync.WaitGroup
		var mu sync.Mutex
		data = make([]byte, h.contentLength)

		for _, ranges := range h.Split() {
			wg.Add(1)
			go func(start, end int) {
				defer wg.Done()
				partData := h.download(start, end)
				mu.Lock()
				copy(data[start:end+1], partData)
				mu.Unlock()
			}(ranges[0], ranges[1])
		}
		wg.Wait()
	}

	return data
}

func (h *HttpDownloader) Split() [][]int {
	ranges := [][]int{}
	blockSize := h.contentLength / h.numThreads
	for i := 0; i < h.numThreads; i++ {
		var start int = i * blockSize
		var end int = (i+1)*blockSize - 1
		if i == h.numThreads-1 {
			end = h.contentLength - 1
		}
		ranges = append(ranges, []int{start, end})
	}
	return ranges
}

func (h *HttpDownloader) download(start, end int) []byte {
	req, err := http.NewRequest("GET", h.url, nil)
	h.check(err)
	req.Header.Set("Range", fmt.Sprintf("bytes=%v-%v", start, end))
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	h.check(err)
	defer resp.Body.Close()

	return h.save2memory(resp)
}

func (h *HttpDownloader) save2memory(resp *http.Response) []byte {
	content, err := ioutil.ReadAll(resp.Body)
	h.check(err)
	return content
}
func extractSong(url string) (*SongInfo, error) {
	start := time.Now()
	d := NewDownload(url, 10)
	rawSong := d.Download()
	if d.err != nil {
		return nil, d.err

	}
	f := bytes.NewReader(rawSong)

	trex, err := mp4.ExtractBoxWithPayload(f, nil, []mp4.BoxType{
		mp4.BoxTypeMoov(),
		mp4.BoxTypeMvex(),
		mp4.BoxTypeTrex(),
	})
	if err != nil || len(trex) != 1 {
		return nil, err
	}
	trexPay := trex[0].Payload.(*mp4.Trex)

	stbl, err := mp4.ExtractBox(f, nil, []mp4.BoxType{
		mp4.BoxTypeMoov(),
		mp4.BoxTypeTrak(),
		mp4.BoxTypeMdia(),
		mp4.BoxTypeMinf(),
		mp4.BoxTypeStbl(),
	})
	if err != nil || len(stbl) != 1 {
		return nil, err
	}

	enca, err := mp4.ExtractBoxWithPayload(f, stbl[0], []mp4.BoxType{
		mp4.BoxTypeStsd(),
		mp4.BoxTypeEnca(),
	})
	if err != nil {
		return nil, err
	}

	aalac, err := mp4.ExtractBoxWithPayload(f, &enca[0].Info,
		[]mp4.BoxType{BoxTypeAlac()})
	if err != nil || len(aalac) != 1 {
		return nil, err
	}

	extracted := &SongInfo{
		r:         f,
		alacParam: aalac[0].Payload.(*Alac),
	}

	moofs, err := mp4.ExtractBox(f, nil, []mp4.BoxType{
		mp4.BoxTypeMoof(),
	})
	if err != nil || len(moofs) <= 0 {
		return nil, err
	}

	mdats, err := mp4.ExtractBoxWithPayload(f, nil, []mp4.BoxType{
		mp4.BoxTypeMdat(),
	})
	if err != nil || len(mdats) != len(moofs) {
		return nil, err
	}

	for i, moof := range moofs {
		tfhd, err := mp4.ExtractBoxWithPayload(f, moof, []mp4.BoxType{
			mp4.BoxTypeTraf(),
			mp4.BoxTypeTfhd(),
		})
		if err != nil || len(tfhd) != 1 {
			return nil, err
		}
		tfhdPay := tfhd[0].Payload.(*mp4.Tfhd)
		index := tfhdPay.SampleDescriptionIndex
		if index != 0 {
			index--
		}

		truns, err := mp4.ExtractBoxWithPayload(f, moof, []mp4.BoxType{
			mp4.BoxTypeTraf(),
			mp4.BoxTypeTrun(),
		})
		if err != nil || len(truns) <= 0 {
			return nil, err
		}

		mdat := mdats[i].Payload.(*mp4.Mdat).Data
		for _, t := range truns {
			for _, en := range t.Payload.(*mp4.Trun).Entries {
				info := SampleInfo{descIndex: index}

				switch {
				case t.Payload.CheckFlag(0x200):
					info.data = mdat[:en.SampleSize]
					mdat = mdat[en.SampleSize:]
				case tfhdPay.CheckFlag(0x10):
					info.data = mdat[:tfhdPay.DefaultSampleSize]
					mdat = mdat[tfhdPay.DefaultSampleSize:]
				default:
					info.data = mdat[:trexPay.DefaultSampleSize]
					mdat = mdat[trexPay.DefaultSampleSize:]
				}

				switch {
				case t.Payload.CheckFlag(0x100):
					info.duration = en.SampleDuration
				case tfhdPay.CheckFlag(0x8):
					info.duration = tfhdPay.DefaultSampleDuration
				default:
					info.duration = trexPay.DefaultSampleDuration
				}

				extracted.samples = append(extracted.samples, info)
			}
		}
		if len(mdat) != 0 {
			return nil, errors.New("offset mismatch")
		}
	}
	end := time.Now()
	fmt.Println("Extracted in", end.Sub(start))
	return extracted, nil
}
func rip(albumId string, token string, storefront string) error {
	var failed bool
	meta, err := getMeta(albumId, token, storefront)
	if err != nil {
		fmt.Println("Failed to get album metadata.\n")
		return err
	}
	albumFolder := fmt.Sprintf("%s - %s", meta.Data[0].Attributes.ArtistName, meta.Data[0].Attributes.Name)
	sanAlbumFolder := filepath.Join("AM-DL downloads", forbiddenNames.ReplaceAllString(albumFolder, "_"))
	os.MkdirAll(sanAlbumFolder, os.ModePerm)
	fmt.Println(albumFolder)
	err = writeCover(sanAlbumFolder, meta.Data[0].Attributes.Artwork.URL)
	if err != nil {
		fmt.Println("Failed to write cover.")
	}
	trackTotal := len(meta.Data[0].Relationships.Tracks.Data)
	for trackNum, track := range meta.Data[0].Relationships.Tracks.Data {
		trackNum++
		fmt.Printf("Track %d of %d:\n", trackNum, trackTotal)
		manifest, err := getInfoFromAdam(track.ID, token, storefront)
		if err != nil {
			failed = true
			fmt.Println("Failed to get manifest.\n", err)
			continue
		}
		if manifest.Attributes.ExtendedAssetUrls.EnhancedHls == "" {
			fmt.Println("Unavailable in ALAC.")
			failed = true
			continue
		}
		filename := fmt.Sprintf("%02d. %s.m4a", trackNum, forbiddenNames.ReplaceAllString(track.Attributes.Name, "_"))
		trackPath := filepath.Join(sanAlbumFolder, filename)
		exists, err := fileExists(trackPath)
		if err != nil {
			failed = true
			fmt.Println("Failed to check if track exists.")
		}
		if exists {
			fmt.Println("Track already exists locally.")
			continue
		}
		trackUrl, keys, err := extractMedia(manifest.Attributes.ExtendedAssetUrls.EnhancedHls)
		if err != nil {
			failed = true
			fmt.Println("Failed to extract info from manifest.\n", err)
			continue
		}
		info, err := extractSong(trackUrl)
		if err != nil {
			failed = true
			fmt.Println("Failed to extract track.", err)
			continue
		}
		samplesOk := true
		for samplesOk {
			for _, i := range info.samples {
				if int(i.descIndex) >= len(keys) {
					fmt.Println("Decryption size mismatch.")
					samplesOk = false
				}
			}
			break
		}
		if !samplesOk {
			continue
		}
		err = decryptSong(info, keys, meta, trackPath, trackNum, trackTotal)
		if err != nil {
			failed = true
			fmt.Println("Failed to decrypt track.\n", err)
			continue
		}
	}
	if failed {
		return errors.New("some tracks failed to download")
	}
	return err
}

func getInfoFromAdam(adamId string, token string, storefront string) (*SongData, error) {
	request, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s", storefront, adamId), nil)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("extend", "extendedAssetUrls")
	query.Set("include", "albums")
	request.URL.RawQuery = query.Encode()

	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	request.Header.Set("User-Agent", "iTunes/12.11.3 (Windows; Microsoft Windows 10 x64 Professional Edition (Build 19041); x64) AppleWebKit/7611.1022.4001.1 (dt:2)")
	request.Header.Set("Origin", "https://music.apple.com")

	do, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		return nil, errors.New(do.Status)
	}

	obj := new(ApiResult)
	err = json.NewDecoder(do.Body).Decode(&obj)
	if err != nil {
		return nil, err
	}

	for _, d := range obj.Data {
		if d.ID == adamId {
			return &d, nil
		}
	}
	return nil, nil
}

func getToken() (string, error) {
	req, err := http.NewRequest("GET", "https://beta.music.apple.com", nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex := regexp.MustCompile(`/assets/index-legacy-[^/]+\.js`)
	indexJsUri := regex.FindString(string(body))

	req, err = http.NewRequest("GET", "https://beta.music.apple.com"+indexJsUri, nil)
	if err != nil {
		return "", err
	}

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex = regexp.MustCompile(`eyJh([^"]*)`)
	token := regex.FindString(string(body))

	return token, nil
}

type ApiResult struct {
	Data []SongData `json:"data"`
}

type SongAttributes struct {
	ArtistName        string   `json:"artistName"`
	DiscNumber        int      `json:"discNumber"`
	GenreNames        []string `json:"genreNames"`
	ExtendedAssetUrls struct {
		EnhancedHls string `json:"enhancedHls"`
	} `json:"extendedAssetUrls"`
	IsMasteredForItunes bool   `json:"isMasteredForItunes"`
	ReleaseDate         string `json:"releaseDate"`
	Name                string `json:"name"`
	Isrc                string `json:"isrc"`
	AlbumName           string `json:"albumName"`
	TrackNumber         int    `json:"trackNumber"`
	ComposerName        string `json:"composerName"`
}

type AlbumAttributes struct {
	ArtistName          string   `json:"artistName"`
	IsSingle            bool     `json:"isSingle"`
	IsComplete          bool     `json:"isComplete"`
	GenreNames          []string `json:"genreNames"`
	TrackCount          int      `json:"trackCount"`
	IsMasteredForItunes bool     `json:"isMasteredForItunes"`
	ReleaseDate         string   `json:"releaseDate"`
	Name                string   `json:"name"`
	RecordLabel         string   `json:"recordLabel"`
	Upc                 string   `json:"upc"`
	Copyright           string   `json:"copyright"`
	IsCompilation       bool     `json:"isCompilation"`
}

type SongData struct {
	ID            string         `json:"id"`
	Attributes    SongAttributes `json:"attributes"`
	Relationships struct {
		Albums struct {
			Data []struct {
				ID         string          `json:"id"`
				Type       string          `json:"type"`
				Href       string          `json:"href"`
				Attributes AlbumAttributes `json:"attributes"`
			} `json:"data"`
		} `json:"albums"`
		Artists struct {
			Href string `json:"href"`
			Data []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Href string `json:"href"`
			} `json:"data"`
		} `json:"artists"`
	} `json:"relationships"`
}

type SongResult struct {
	Artwork struct {
		Width                int    `json:"width"`
		URL                  string `json:"url"`
		Height               int    `json:"height"`
		TextColor3           string `json:"textColor3"`
		TextColor2           string `json:"textColor2"`
		TextColor4           string `json:"textColor4"`
		HasAlpha             bool   `json:"hasAlpha"`
		TextColor1           string `json:"textColor1"`
		BgColor              string `json:"bgColor"`
		HasP3                bool   `json:"hasP3"`
		SupportsLayeredImage bool   `json:"supportsLayeredImage"`
	} `json:"artwork"`
	ArtistName             string   `json:"artistName"`
	CollectionID           string   `json:"collectionId"`
	DiscNumber             int      `json:"discNumber"`
	GenreNames             []string `json:"genreNames"`
	ID                     string   `json:"id"`
	DurationInMillis       int      `json:"durationInMillis"`
	ReleaseDate            string   `json:"releaseDate"`
	ContentRatingsBySystem struct {
	} `json:"contentRatingsBySystem"`
	Name     string `json:"name"`
	Composer struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"composer"`
	EditorialArtwork struct {
	} `json:"editorialArtwork"`
	CollectionName string `json:"collectionName"`
	AssetUrls      struct {
		Plus             string `json:"plus"`
		Lightweight      string `json:"lightweight"`
		SuperLightweight string `json:"superLightweight"`
		LightweightPlus  string `json:"lightweightPlus"`
		EnhancedHls      string `json:"enhancedHls"`
	} `json:"assetUrls"`
	AudioTraits []string `json:"audioTraits"`
	Kind        string   `json:"kind"`
	Copyright   string   `json:"copyright"`
	ArtistID    string   `json:"artistId"`
	Genres      []struct {
		GenreID   string `json:"genreId"`
		Name      string `json:"name"`
		URL       string `json:"url"`
		MediaType string `json:"mediaType"`
	} `json:"genres"`
	TrackNumber int    `json:"trackNumber"`
	AudioLocale string `json:"audioLocale"`
	Offers      []struct {
		ActionText struct {
			Short       string `json:"short"`
			Medium      string `json:"medium"`
			Long        string `json:"long"`
			Downloaded  string `json:"downloaded"`
			Downloading string `json:"downloading"`
		} `json:"actionText"`
		Type           string  `json:"type"`
		PriceFormatted string  `json:"priceFormatted"`
		Price          float64 `json:"price"`
		BuyParams      string  `json:"buyParams"`
		Variant        string  `json:"variant,omitempty"`
		Assets         []struct {
			Flavor  string `json:"flavor"`
			Preview struct {
				Duration int    `json:"duration"`
				URL      string `json:"url"`
			} `json:"preview"`
			Size     int `json:"size"`
			Duration int `json:"duration"`
		} `json:"assets"`
	} `json:"offers"`
}
type iTunesLookup struct {
	Results map[string]SongResult `json:"results"`
}

type Meta struct {
	Context     string `json:"@context"`
	Type        string `json:"@type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Tracks      []struct {
		Type  string `json:"@type"`
		Name  string `json:"name"`
		Audio struct {
			Type string `json:"@type"`
		} `json:"audio"`
		Offers struct {
			Type     string `json:"@type"`
			Category string `json:"category"`
			Price    int    `json:"price"`
		} `json:"offers"`
		Duration string `json:"duration"`
	} `json:"tracks"`
	Citation    []interface{} `json:"citation"`
	WorkExample []struct {
		Type  string `json:"@type"`
		Name  string `json:"name"`
		URL   string `json:"url"`
		Audio struct {
			Type string `json:"@type"`
		} `json:"audio"`
		Offers struct {
			Type     string `json:"@type"`
			Category string `json:"category"`
			Price    int    `json:"price"`
		} `json:"offers"`
		Duration string `json:"duration"`
	} `json:"workExample"`
	Genre         []string  `json:"genre"`
	DatePublished time.Time `json:"datePublished"`
	ByArtist      struct {
		Type string `json:"@type"`
		URL  string `json:"url"`
		Name string `json:"name"`
	} `json:"byArtist"`
}

type AutoGenerated struct {
	Data []struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Href       string `json:"href"`
		Attributes struct {
			Artwork struct {
				Width      int    `json:"width"`
				Height     int    `json:"height"`
				URL        string `json:"url"`
				BgColor    string `json:"bgColor"`
				TextColor1 string `json:"textColor1"`
				TextColor2 string `json:"textColor2"`
				TextColor3 string `json:"textColor3"`
				TextColor4 string `json:"textColor4"`
			} `json:"artwork"`
			ArtistName          string   `json:"artistName"`
			IsSingle            bool     `json:"isSingle"`
			URL                 string   `json:"url"`
			IsComplete          bool     `json:"isComplete"`
			GenreNames          []string `json:"genreNames"`
			TrackCount          int      `json:"trackCount"`
			IsMasteredForItunes bool     `json:"isMasteredForItunes"`
			ReleaseDate         string   `json:"releaseDate"`
			Name                string   `json:"name"`
			RecordLabel         string   `json:"recordLabel"`
			Upc                 string   `json:"upc"`
			AudioTraits         []string `json:"audioTraits"`
			Copyright           string   `json:"copyright"`
			PlayParams          struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"playParams"`
			IsCompilation bool `json:"isCompilation"`
		} `json:"attributes"`
		Relationships struct {
			RecordLabels struct {
				Href string        `json:"href"`
				Data []interface{} `json:"data"`
			} `json:"record-labels"`
			Artists struct {
				Href string `json:"href"`
				Data []struct {
					ID         string `json:"id"`
					Type       string `json:"type"`
					Href       string `json:"href"`
					Attributes struct {
						Name string `json:"name"`
					} `json:"attributes"`
				} `json:"data"`
			} `json:"artists"`
			Tracks struct {
				Href string `json:"href"`
				Data []struct {
					ID         string `json:"id"`
					Type       string `json:"type"`
					Href       string `json:"href"`
					Attributes struct {
						Previews []struct {
							URL string `json:"url"`
						} `json:"previews"`
						Artwork struct {
							Width      int    `json:"width"`
							Height     int    `json:"height"`
							URL        string `json:"url"`
							BgColor    string `json:"bgColor"`
							TextColor1 string `json:"textColor1"`
							TextColor2 string `json:"textColor2"`
							TextColor3 string `json:"textColor3"`
							TextColor4 string `json:"textColor4"`
						} `json:"artwork"`
						ArtistName          string   `json:"artistName"`
						URL                 string   `json:"url"`
						DiscNumber          int      `json:"discNumber"`
						GenreNames          []string `json:"genreNames"`
						HasTimeSyncedLyrics bool     `json:"hasTimeSyncedLyrics"`
						IsMasteredForItunes bool     `json:"isMasteredForItunes"`
						DurationInMillis    int      `json:"durationInMillis"`
						ReleaseDate         string   `json:"releaseDate"`
						Name                string   `json:"name"`
						Isrc                string   `json:"isrc"`
						AudioTraits         []string `json:"audioTraits"`
						HasLyrics           bool     `json:"hasLyrics"`
						AlbumName           string   `json:"albumName"`
						PlayParams          struct {
							ID   string `json:"id"`
							Kind string `json:"kind"`
						} `json:"playParams"`
						TrackNumber  int    `json:"trackNumber"`
						AudioLocale  string `json:"audioLocale"`
						ComposerName string `json:"composerName"`
					} `json:"attributes"`
					Relationships struct {
						Artists struct {
							Href string `json:"href"`
							Data []struct {
								ID         string `json:"id"`
								Type       string `json:"type"`
								Href       string `json:"href"`
								Attributes struct {
									Name string `json:"name"`
								} `json:"attributes"`
							} `json:"data"`
						} `json:"artists"`
					} `json:"relationships"`
				} `json:"data"`
			} `json:"tracks"`
		} `json:"relationships"`
	} `json:"data"`
}
type SampleInfo struct {
	data      []byte
	duration  uint32
	descIndex uint32
}

type SongInfo struct {
	r         io.ReadSeeker
	alacParam *Alac
	samples   []SampleInfo
}

func BoxTypeAlac() mp4.BoxType { return mp4.StrToBoxType("alac") }
func init() {
	mp4.AddBoxDef((*Alac)(nil))
}

type Alac struct {
	mp4.FullBox `mp4:"extend"`

	FrameLength       uint32 `mp4:"size=32"`
	CompatibleVersion uint8  `mp4:"size=8"`
	BitDepth          uint8  `mp4:"size=8"`
	Pb                uint8  `mp4:"size=8"`
	Mb                uint8  `mp4:"size=8"`
	Kb                uint8  `mp4:"size=8"`
	NumChannels       uint8  `mp4:"size=8"`
	MaxRun            uint16 `mp4:"size=16"`
	MaxFrameBytes     uint32 `mp4:"size=32"`
	AvgBitRate        uint32 `mp4:"size=32"`
	SampleRate        uint32 `mp4:"size=32"`
}

func (s *SongInfo) Duration() (ret uint64) {
	for i := range s.samples {
		ret += uint64(s.samples[i].duration)
	}
	return
}

func (*Alac) GetType() mp4.BoxType {
	return BoxTypeAlac()
}

func fileExists(path string) (bool, error) {
	f, err := os.Stat(path)
	if err == nil {
		return !f.IsDir(), nil
	} else if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func writeM4a(w *mp4.Writer, info *SongInfo, meta *AutoGenerated, data []byte, trackNum, trackTotal int) error {
	index := trackNum - 1
	{ // ftyp
		box, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()})
		if err != nil {
			return err
		}
		_, err = mp4.Marshal(w, &mp4.Ftyp{
			MajorBrand:   [4]byte{'M', '4', 'A', ' '},
			MinorVersion: 0,
			CompatibleBrands: []mp4.CompatibleBrandElem{
				{CompatibleBrand: [4]byte{'M', '4', 'A', ' '}},
				{CompatibleBrand: [4]byte{'m', 'p', '4', '2'}},
				{CompatibleBrand: mp4.BrandISOM()},
				{CompatibleBrand: [4]byte{0, 0, 0, 0}},
			},
		}, box.Context)
		if err != nil {
			return err
		}
		_, err = w.EndBox()
		if err != nil {
			return err
		}
	}

	const chunkSize uint32 = 5
	duration := info.Duration()
	numSamples := uint32(len(info.samples))
	var stco *mp4.BoxInfo

	{ // moov
		_, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()})
		if err != nil {
			return err
		}
		box, err := mp4.ExtractBox(info.r, nil, mp4.BoxPath{mp4.BoxTypeMoov()})
		if err != nil {
			return err
		}
		moovOri := box[0]

		{ // mvhd
			_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMvhd()})
			if err != nil {
				return err
			}

			oriBox, err := mp4.ExtractBoxWithPayload(info.r, moovOri, mp4.BoxPath{mp4.BoxTypeMvhd()})
			if err != nil {
				return err
			}
			mvhd := oriBox[0].Payload.(*mp4.Mvhd)
			if mvhd.Version == 0 {
				mvhd.DurationV0 = uint32(duration)
			} else {
				mvhd.DurationV1 = duration
			}

			_, err = mp4.Marshal(w, mvhd, oriBox[0].Info.Context)
			if err != nil {
				return err
			}

			_, err = w.EndBox()
			if err != nil {
				return err
			}
		}

		{ // trak
			_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()})
			if err != nil {
				return err
			}

			box, err := mp4.ExtractBox(info.r, moovOri, mp4.BoxPath{mp4.BoxTypeTrak()})
			if err != nil {
				return err
			}
			trakOri := box[0]

			{ // tkhd
				_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTkhd()})
				if err != nil {
					return err
				}

				oriBox, err := mp4.ExtractBoxWithPayload(info.r, trakOri, mp4.BoxPath{mp4.BoxTypeTkhd()})
				if err != nil {
					return err
				}
				tkhd := oriBox[0].Payload.(*mp4.Tkhd)
				if tkhd.Version == 0 {
					tkhd.DurationV0 = uint32(duration)
				} else {
					tkhd.DurationV1 = duration
				}
				tkhd.SetFlags(0x7)

				_, err = mp4.Marshal(w, tkhd, oriBox[0].Info.Context)
				if err != nil {
					return err
				}

				_, err = w.EndBox()
				if err != nil {
					return err
				}
			}

			{ // mdia
				_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()})
				if err != nil {
					return err
				}

				box, err := mp4.ExtractBox(info.r, trakOri, mp4.BoxPath{mp4.BoxTypeMdia()})
				if err != nil {
					return err
				}
				mdiaOri := box[0]

				{ // mdhd
					_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdhd()})
					if err != nil {
						return err
					}

					oriBox, err := mp4.ExtractBoxWithPayload(info.r, mdiaOri, mp4.BoxPath{mp4.BoxTypeMdhd()})
					if err != nil {
						return err
					}
					mdhd := oriBox[0].Payload.(*mp4.Mdhd)
					if mdhd.Version == 0 {
						mdhd.DurationV0 = uint32(duration)
					} else {
						mdhd.DurationV1 = duration
					}

					_, err = mp4.Marshal(w, mdhd, oriBox[0].Info.Context)
					if err != nil {
						return err
					}

					_, err = w.EndBox()
					if err != nil {
						return err
					}
				}

				{ // hdlr
					oriBox, err := mp4.ExtractBox(info.r, mdiaOri, mp4.BoxPath{mp4.BoxTypeHdlr()})
					if err != nil {
						return err
					}

					err = w.CopyBox(info.r, oriBox[0])
					if err != nil {
						return err
					}
				}

				{ // minf
					_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()})
					if err != nil {
						return err
					}

					box, err := mp4.ExtractBox(info.r, mdiaOri, mp4.BoxPath{mp4.BoxTypeMinf()})
					if err != nil {
						return err
					}
					minfOri := box[0]

					{ // smhd, dinf
						boxes, err := mp4.ExtractBoxes(info.r, minfOri, []mp4.BoxPath{
							{mp4.BoxTypeSmhd()},
							{mp4.BoxTypeDinf()},
						})
						if err != nil {
							return err
						}

						for _, b := range boxes {
							err = w.CopyBox(info.r, b)
							if err != nil {
								return err
							}
						}
					}

					{ // stbl
						_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()})
						if err != nil {
							return err
						}

						{ // stsd
							box, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()})
							if err != nil {
								return err
							}
							_, err = mp4.Marshal(w, &mp4.Stsd{EntryCount: 1}, box.Context)
							if err != nil {
								return err
							}

							{ // alac
								_, err = w.StartBox(&mp4.BoxInfo{Type: BoxTypeAlac()})
								if err != nil {
									return err
								}

								_, err = w.Write([]byte{
									0, 0, 0, 0, 0, 0, 0, 1,
									0, 0, 0, 0, 0, 0, 0, 0})
								if err != nil {
									return err
								}

								err = binary.Write(w, binary.BigEndian, uint16(info.alacParam.NumChannels))
								if err != nil {
									return err
								}

								err = binary.Write(w, binary.BigEndian, uint16(info.alacParam.BitDepth))
								if err != nil {
									return err
								}

								_, err = w.Write([]byte{0, 0})
								if err != nil {
									return err
								}

								err = binary.Write(w, binary.BigEndian, info.alacParam.SampleRate)
								if err != nil {
									return err
								}

								_, err = w.Write([]byte{0, 0})
								if err != nil {
									return err
								}

								box, err := w.StartBox(&mp4.BoxInfo{Type: BoxTypeAlac()})
								if err != nil {
									return err
								}

								_, err = mp4.Marshal(w, info.alacParam, box.Context)
								if err != nil {
									return err
								}

								_, err = w.EndBox()
								if err != nil {
									return err
								}

								_, err = w.EndBox()
								if err != nil {
									return err
								}
							}

							_, err = w.EndBox()
							if err != nil {
								return err
							}
						}

						{ // stts
							box, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStts()})
							if err != nil {
								return err
							}

							var stts mp4.Stts
							for _, sample := range info.samples {
								if len(stts.Entries) != 0 {
									last := &stts.Entries[len(stts.Entries)-1]
									if last.SampleDelta == sample.duration {
										last.SampleCount++
										continue
									}
								}
								stts.Entries = append(stts.Entries, mp4.SttsEntry{
									SampleCount: 1,
									SampleDelta: sample.duration,
								})
							}
							stts.EntryCount = uint32(len(stts.Entries))

							_, err = mp4.Marshal(w, &stts, box.Context)
							if err != nil {
								return err
							}

							_, err = w.EndBox()
							if err != nil {
								return err
							}
						}

						{ // stsc
							box, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsc()})
							if err != nil {
								return err
							}

							if numSamples%chunkSize == 0 {
								_, err = mp4.Marshal(w, &mp4.Stsc{
									EntryCount: 1,
									Entries: []mp4.StscEntry{
										{
											FirstChunk:             1,
											SamplesPerChunk:        chunkSize,
											SampleDescriptionIndex: 1,
										},
									},
								}, box.Context)
							} else {
								_, err = mp4.Marshal(w, &mp4.Stsc{
									EntryCount: 2,
									Entries: []mp4.StscEntry{
										{
											FirstChunk:             1,
											SamplesPerChunk:        chunkSize,
											SampleDescriptionIndex: 1,
										}, {
											FirstChunk:             numSamples/chunkSize + 1,
											SamplesPerChunk:        numSamples % chunkSize,
											SampleDescriptionIndex: 1,
										},
									},
								}, box.Context)
							}

							_, err = w.EndBox()
							if err != nil {
								return err
							}
						}

						{ // stsz
							box, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsz()})
							if err != nil {
								return err
							}

							stsz := mp4.Stsz{SampleCount: numSamples}
							for _, sample := range info.samples {
								stsz.EntrySize = append(stsz.EntrySize, uint32(len(sample.data)))
							}

							_, err = mp4.Marshal(w, &stsz, box.Context)
							if err != nil {
								return err
							}

							_, err = w.EndBox()
							if err != nil {
								return err
							}
						}

						{ // stco
							box, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStco()})
							if err != nil {
								return err
							}

							l := (numSamples + chunkSize - 1) / chunkSize
							_, err = mp4.Marshal(w, &mp4.Stco{
								EntryCount:  l,
								ChunkOffset: make([]uint32, l),
							}, box.Context)

							stco, err = w.EndBox()
							if err != nil {
								return err
							}
						}

						_, err = w.EndBox()
						if err != nil {
							return err
						}
					}

					_, err = w.EndBox()
					if err != nil {
						return err
					}
				}

				_, err = w.EndBox()
				if err != nil {
					return err
				}
			}

			_, err = w.EndBox()
			if err != nil {
				return err
			}
		}

		{ // udta
			ctx := mp4.Context{UnderUdta: true}
			_, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUdta(), Context: ctx})
			if err != nil {
				return err
			}

			{ // meta
				ctx.UnderIlstMeta = true

				_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMeta(), Context: ctx})
				if err != nil {
					return err
				}

				_, err = mp4.Marshal(w, &mp4.Meta{}, ctx)
				if err != nil {
					return err
				}

				{ // hdlr
					_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeHdlr(), Context: ctx})
					if err != nil {
						return err
					}

					_, err = mp4.Marshal(w, &mp4.Hdlr{
						HandlerType: [4]byte{'m', 'd', 'i', 'r'},
						Reserved:    [3]uint32{0x6170706c, 0, 0},
					}, ctx)
					if err != nil {
						return err
					}

					_, err = w.EndBox()
					if err != nil {
						return err
					}
				}

				{ // ilst
					ctx.UnderIlst = true

					_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeIlst(), Context: ctx})
					if err != nil {
						return err
					}

					marshalData := func(val interface{}) error {
						_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeData()})
						if err != nil {
							return err
						}

						var boxData mp4.Data
						switch v := val.(type) {
						case string:
							boxData.DataType = mp4.DataTypeStringUTF8
							boxData.Data = []byte(v)
						case uint8:
							boxData.DataType = mp4.DataTypeSignedIntBigEndian
							boxData.Data = []byte{v}
						case uint32:
							boxData.DataType = mp4.DataTypeSignedIntBigEndian
							boxData.Data = make([]byte, 4)
							binary.BigEndian.PutUint32(boxData.Data, v)
						case []byte:
							boxData.DataType = mp4.DataTypeBinary
							boxData.Data = v
						default:
							panic("unsupported value")
						}

						_, err = mp4.Marshal(w, &boxData, ctx)
						if err != nil {
							return err
						}

						_, err = w.EndBox()
						return err
					}

					addMeta := func(tag mp4.BoxType, val interface{}) error {
						_, err = w.StartBox(&mp4.BoxInfo{Type: tag})
						if err != nil {
							return err
						}

						err = marshalData(val)
						if err != nil {
							return err
						}

						_, err = w.EndBox()
						return err
					}

					addExtendedMeta := func(name string, val interface{}) error {
						ctx.UnderIlstFreeMeta = true

						_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxType{'-', '-', '-', '-'}, Context: ctx})
						if err != nil {
							return err
						}

						{
							_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxType{'m', 'e', 'a', 'n'}, Context: ctx})
							if err != nil {
								return err
							}

							_, err = w.Write([]byte{0, 0, 0, 0})
							if err != nil {
								return err
							}

							_, err = io.WriteString(w, "com.apple.iTunes")
							if err != nil {
								return err
							}

							_, err = w.EndBox()
							if err != nil {
								return err
							}
						}

						{
							_, err = w.StartBox(&mp4.BoxInfo{Type: mp4.BoxType{'n', 'a', 'm', 'e'}, Context: ctx})
							if err != nil {
								return err
							}

							_, err = w.Write([]byte{0, 0, 0, 0})
							if err != nil {
								return err
							}

							_, err = io.WriteString(w, name)
							if err != nil {
								return err
							}

							_, err = w.EndBox()
							if err != nil {
								return err
							}
						}

						err = marshalData(val)
						if err != nil {
							return err
						}

						ctx.UnderIlstFreeMeta = false

						_, err = w.EndBox()
						return err
					}

					err = addMeta(mp4.BoxType{'\251', 'n', 'a', 'm'}, meta.Data[0].Relationships.Tracks.Data[index].Attributes.Name)
					if err != nil {
						return err
					}

					err = addMeta(mp4.BoxType{'\251', 'a', 'l', 'b'}, meta.Data[0].Attributes.Name)
					if err != nil {
						return err
					}

					err = addMeta(mp4.BoxType{'\251', 'A', 'R', 'T'}, meta.Data[0].Relationships.Tracks.Data[index].Attributes.ArtistName)
					if err != nil {
						return err
					}

					err = addMeta(mp4.BoxType{'\251', 'w', 'r', 't'}, meta.Data[0].Relationships.Tracks.Data[index].Attributes.ComposerName)
					if err != nil {
						return err
					}

					err = addMeta(mp4.BoxType{'\251', 'd', 'a', 'y'}, strings.Split(meta.Data[0].Attributes.ReleaseDate, "-")[0])
					if err != nil {
						return err
					}

					// cnID, err := strconv.ParseUint(meta.Data[0].Relationships.Tracks.Data[index].ID, 10, 32)
					// if err != nil {
					// 	return err
					// }

					// err = addMeta(mp4.BoxType{'c', 'n', 'I', 'D'}, uint32(cnID))
					// if err != nil {
					// 	return err
					// }

					err = addExtendedMeta("ISRC", meta.Data[0].Relationships.Tracks.Data[index].Attributes.Isrc)
					if err != nil {
						return err
					}

					if len(meta.Data[0].Relationships.Tracks.Data[index].Attributes.GenreNames) > 0 {
						err = addMeta(mp4.BoxType{'\251', 'g', 'e', 'n'}, meta.Data[0].Relationships.Tracks.Data[index].Attributes.GenreNames[0])
						if err != nil {
							return err
						}
					}

					if len(meta.Data) > 0 {
						album := meta.Data[0]

						err = addMeta(mp4.BoxType{'a', 'A', 'R', 'T'}, album.Attributes.ArtistName)
						if err != nil {
							return err
						}

						err = addMeta(mp4.BoxType{'c', 'p', 'r', 't'}, album.Attributes.Copyright)
						if err != nil {
							return err
						}

						var isCpil uint8
						if album.Attributes.IsCompilation {
							isCpil = 1
						}
						err = addMeta(mp4.BoxType{'c', 'p', 'i', 'l'}, isCpil)
						if err != nil {
							return err
						}

						err = addExtendedMeta("LABEL", album.Attributes.RecordLabel)
						if err != nil {
							return err
						}

						err = addExtendedMeta("UPC", album.Attributes.Upc)
						if err != nil {
							return err
						}

						// plID, err := strconv.ParseUint(album.ID, 10, 32)
						// if err != nil {
						// 	return err
						// }

						// err = addMeta(mp4.BoxType{'p', 'l', 'I', 'D'}, uint32(plID))
						// if err != nil {
						// 	return err
						// }
					}

					// if len(meta.Data[0].Relationships.Artists.Data) > 0 {
					// 	atID, err := strconv.ParseUint(meta.Data[0].Relationships.Artists.Data[index].ID, 10, 32)
					// 	if err != nil {
					// 		return err
					// 	}

					// 	err = addMeta(mp4.BoxType{'a', 't', 'I', 'D'}, uint32(atID))
					// 	if err != nil {
					// 		return err
					// 	}
					// }

					trkn := make([]byte, 8)
					binary.BigEndian.PutUint32(trkn, uint32(trackNum))
					binary.BigEndian.PutUint16(trkn[4:], uint16(trackTotal))
					err = addMeta(mp4.BoxType{'t', 'r', 'k', 'n'}, trkn)
					if err != nil {
						return err
					}

					// disk := make([]byte, 8)
					// binary.BigEndian.PutUint32(disk, uint32(meta.Attributes.DiscNumber))
					// err = addMeta(mp4.BoxType{'d', 'i', 's', 'k'}, disk)
					// if err != nil {
					// 	return err
					// }

					ctx.UnderIlst = false

					_, err = w.EndBox()
					if err != nil {
						return err
					}
				}

				ctx.UnderIlstMeta = false
				_, err = w.EndBox()
				if err != nil {
					return err
				}
			}

			ctx.UnderUdta = false
			_, err = w.EndBox()
			if err != nil {
				return err
			}
		}

		_, err = w.EndBox()
		if err != nil {
			return err
		}
	}

	{
		box, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()})
		if err != nil {
			return err
		}

		_, err = mp4.Marshal(w, &mp4.Mdat{Data: data}, box.Context)
		if err != nil {
			return err
		}

		mdat, err := w.EndBox()

		var realStco mp4.Stco

		offset := mdat.Offset + mdat.HeaderSize
		for i := uint32(0); i < numSamples; i++ {
			if i%chunkSize == 0 {
				realStco.EntryCount++
				realStco.ChunkOffset = append(realStco.ChunkOffset, uint32(offset))
			}
			offset += uint64(len(info.samples[i].data))
		}

		_, err = stco.SeekToPayload(w)
		if err != nil {
			return err
		}
		_, err = mp4.Marshal(w, &realStco, box.Context)
		if err != nil {
			return err
		}
	}

	return nil
}

func decryptSong(info *SongInfo, keys []string, manifest *AutoGenerated, filename string, trackNum, trackTotal int) error {
	//fmt.Printf("%d-bit / %d Hz\n", info.bitDepth, info.bitRate)
	conn, err := net.Dial("tcp", "127.0.0.1:10020")
	if err != nil {
		return err
	}
	defer conn.Close()
	var decrypted []byte
	var lastIndex uint32 = math.MaxUint8

	fmt.Println("Decrypt start.")
	for _, sp := range info.samples {
		if lastIndex != sp.descIndex {
			if len(decrypted) != 0 {
				_, err := conn.Write([]byte{0, 0, 0, 0})
				if err != nil {
					return err
				}
			}
			keyUri := keys[sp.descIndex]
			id := manifest.Data[0].Relationships.Tracks.Data[trackNum-1].ID
			if keyUri == prefetchKey {
				id = defaultId
			}

			_, err := conn.Write([]byte{byte(len(id))})
			if err != nil {
				return err
			}
			_, err = io.WriteString(conn, id)
			if err != nil {
				return err
			}

			_, err = conn.Write([]byte{byte(len(keyUri))})
			if err != nil {
				return err
			}
			_, err = io.WriteString(conn, keyUri)
			if err != nil {
				return err
			}
		}
		lastIndex = sp.descIndex

		err := binary.Write(conn, binary.LittleEndian, uint32(len(sp.data)))
		if err != nil {
			return err
		}

		_, err = conn.Write(sp.data)
		if err != nil {
			return err
		}

		de := make([]byte, len(sp.data))
		_, err = io.ReadFull(conn, de)
		if err != nil {
			return err
		}

		decrypted = append(decrypted, de...)
	}
	_, _ = conn.Write([]byte{0, 0, 0, 0, 0})

	fmt.Println("Decrypt finished.")

	create, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer create.Close()

	return writeM4a(mp4.NewWriter(create), info, manifest, decrypted, trackNum, trackTotal)
}

func checkUrl(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/album|\/album\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)
	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}

func getMeta(albumId string, token string, storefront string) (*AutoGenerated, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/albums/%s", storefront, albumId), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Origin", "https://music.apple.com")
	query := url.Values{}
	query.Set("omit[resource]", "autos")
	query.Set("include", "tracks,artists,record-labels")
	query.Set("include[songs]", "artists")
	query.Set("fields[artists]", "name")
	query.Set("fields[albums:albums]", "artistName,artwork,name,releaseDate,url")
	query.Set("fields[record-labels]", "name")
	// query.Set("l", "en-gb")
	req.URL.RawQuery = query.Encode()
	do, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		return nil, errors.New(do.Status)
	}
	obj := new(AutoGenerated)
	err = json.NewDecoder(do.Body).Decode(&obj)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func writeCover(sanAlbumFolder, url string) error {
	covPath := filepath.Join(sanAlbumFolder, "cover.jpg")
	exists, err := fileExists(covPath)
	if err != nil {
		fmt.Println("Failed to check if cover exists.")
		return err
	}
	if exists {
		return nil
	}
	url = strings.Replace(url, "{w}x{h}", "1200x12000", 1)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	do, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		errors.New(do.Status)
	}
	f, err := os.Create(covPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, do.Body)
	if err != nil {
		return err
	}
	return nil
}

func extractMedia(b string) (string, []string, error) {
	masterUrl, err := url.Parse(b)
	if err != nil {
		return "", nil, err
	}
	resp, err := http.Get(b)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, errors.New(resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	masterString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(masterString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", nil, errors.New("m3u8 not of master type")
	}
	master := from.(*m3u8.MasterPlaylist)
	var streamUrl *url.URL
	sort.Slice(master.Variants, func(i, j int) bool {
		return master.Variants[i].AverageBandwidth > master.Variants[j].AverageBandwidth
	})
	for _, variant := range master.Variants {
		if variant.Codecs == "alac" {
			split := strings.Split(variant.Audio, "-")
			length := len(split)
			fmt.Printf("%s-bit / %s Hz\n", split[length-1], split[length-2])
			streamUrlTemp, err := masterUrl.Parse(variant.URI)
			if err != nil {
				panic(err)
			}
			streamUrl = streamUrlTemp
			break
		}
	}
	if streamUrl == nil {
		return "", nil, errors.New("no alac codec found")
	}
	var keys []string
	keys = append(keys, prefetchKey)
	streamUrl.Path = strings.TrimSuffix(streamUrl.Path, ".m3u8") + "_m.mp4"
	regex := regexp.MustCompile(`"(skd?://[^"]*)"`)
	matches := regex.FindAllStringSubmatch(masterString, -1)
	for _, match := range matches {
		if strings.HasSuffix(match[1], "c23") || strings.HasSuffix(match[1], "c6") {
			keys = append(keys, match[1])
		}
	}
	return streamUrl.String(), keys, nil
}
func Download(albumId, storefront string) (err error) {
	err = rip(albumId, token, storefront)
	if err != nil {
		fmt.Println("Album failed.")
		fmt.Println(err)
	}
	return
}

var (
	token     string
	taskQueue = make(chan []string)
	failQueue = make(chan []string)
	succQueue = make(chan []string)
	DeConfig  = Config{
		FridaPath:       "frida",
		FridaServerPath: "/data/local/tmp/frida-server-16.2.1-android-x86_64",
		Port:            "8080",
	}
)

type Config struct {
	FridaPath       string `yaml:"frida_path"`
	FridaServerPath string `yaml:"frida_server_path"`
	Port            string `yaml:"port"`
}

func ReadConfig() (config Config, err error) {
	yamlFile, err := ioutil.ReadFile("config.yaml")
	if err != nil {
		return
	}
	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		return
	}
	if config.FridaPath == "" {
		config.FridaPath = "frida"
	}
	if config.FridaServerPath == "" {
		config.FridaServerPath = "/data/local/tmp/frida-server-16.2.1-android-x86_64"

	}
	if config.Port == "" {
		config.Port = "8080"
	}
	return

}
func (c *Config) InitFrida() (err error) {
	fr := exec.Command("adb", "forward", "tcp:10020", "tcp:10020")
	err = runCmd(fr)
	if err != nil {
		return
	}
	frida := exec.Command("adb", "shell", "su", "0", c.FridaServerPath, "&")
	err = runCmd(frida)
	if err != nil && !strings.Contains(err.Error(), "already in use") {
		return
	}
	script := exec.Command(c.FridaPath, "-U", "-l", "agent.js", "-f", "com.apple.android.music")
	err = runCmd(script)
	if err != nil {
		return
	}
	return
}
func runCmd(cmd *exec.Cmd) (err error) {
	fmt.Println(cmd.String())
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		err = errors.New(stderr.String())
	}
	return
}
func InitGin() (err error) {
	token, err = getToken()
	if err != nil {
		fmt.Println("Failed to get token.")
		return
	}
	return
}
func main() {
	config, err := ReadConfig()
	if err != nil {
		config = DeConfig
	}
	go func() {
		err = config.InitFrida()
		if err != nil {
			panic(err)
			return
		}
	}()
	err = InitGin()
	if err != nil {
		fmt.Println(err)
		return
	}
	r := gin.Default()
	applemusic := r.Group("/applemusic")
	applemusic.GET("/addDownload", func(c *gin.Context) {
		url := c.Query("url")
		if url == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "url not provided"})
			return
		}
		storefront, albumId := checkUrl(url)
		if albumId == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
			return
		}
		taskQueue <- []string{url, albumId, storefront}
		c.JSON(http.StatusOK, gin.H{"message": "download added"})
		return
	})
	applemusic.GET("/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "status", "taskQueue": len(taskQueue), "failQueue": len(failQueue), "succQueue": len(succQueue)})
		return
	})
	applemusic.GET("/fail", func(c *gin.Context) {
		var failList []string
		for i := 0; i < len(failQueue); i++ {
			failList = append(failList, <-failQueue...)
		}
		c.JSON(http.StatusOK, gin.H{"failList": failList})
		return

	})
	go func() {
		for {
			select {
			case task := <-taskQueue:
				err := Download(task[1], task[2])
				if err != nil {
					task = append(task, err.Error())
					failQueue <- task
				} else {
					succQueue <- task
				}
			}
		}
	}()
	err = r.Run(":" + config.Port)
	if err != nil {
		return
	}
}

//启动模拟器
//查看列表 emulator -list-avds
//emulator @avdname
