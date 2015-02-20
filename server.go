// Implements a server for distributing Cesium terrain tilesets
package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/bradfitz/gomemcache/memcache"
	"github.com/geo-data/cesium-terrain-server/assets"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// Representation of a terrain tile. This includes the x, y, z coordinate and
// the byte sequence of the tile itself. Note that terrain tiles are gzipped.
type Terrain struct {
	x, y, z uint64
	body    []byte
}

// IsRoot returns true if the tile represents a root tile.
func (self *Terrain) IsRoot() bool {
	return self.z == 0 &&
		(self.x == 0 || self.x == 1) &&
		self.y == 0
}

// Parse x, y, z string coordinates and assign them to the Terrain
func (self *Terrain) parseCoord(x, y, z string) error {
	xi, err := strconv.ParseUint(x, 10, 64)
	if err != nil {
		return err
	}

	yi, err := strconv.ParseUint(y, 10, 64)
	if err != nil {
		return err
	}

	zi, err := strconv.ParseUint(z, 10, 64)
	if err != nil {
		return err
	}

	self.x = xi
	self.y = yi
	self.z = zi

	return nil
}

var ErrNoTile = errors.New("tile not found")

type Storer interface {
	Load(tileset string, tile *Terrain) error
	Save(tileset string, tile *Terrain) error
}

type FileStore struct {
	root string
}

func NewFileStore(root string) Storer {
	return &FileStore{
		root: root,
	}
}

// This is a no-op
func (this *FileStore) Save(tileset string, tile *Terrain) error {
	log.Printf("save fs: %s", tileset)
	return nil
}

// Load a terrain tile on disk into the Terrain structure.
func (this *FileStore) Load(tileset string, tile *Terrain) (err error) {
	filename := filepath.Join(
		this.root,
		tileset,
		strconv.FormatUint(tile.z, 10),
		strconv.FormatUint(tile.x, 10),
		strconv.FormatUint(tile.y, 10)+".terrain")
	body, err := ioutil.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			err = ErrNoTile
		}
		return
	}
	log.Printf("load fs: %s", filename)
	tile.body = body
	return
}

type MemcacheStore struct {
	mc *memcache.Client
}

func NewMemcacheStore(connstr string) Storer {
	return &MemcacheStore{
		mc: memcache.New(connstr),
	}
}

func (this *MemcacheStore) key(tileset string, tile *Terrain) string {
	return fmt.Sprintf("%s/%d/%d/%d", tileset, tile.z, tile.x, tile.y)
}

func (this *MemcacheStore) Save(tileset string, tile *Terrain) (err error) {
	key := this.key(tileset, tile)
	log.Printf("save mem: %s", key)
	return this.mc.Set(&memcache.Item{Key: key, Value: tile.body})
}

func (this *MemcacheStore) Load(tileset string, tile *Terrain) (err error) {
	key := this.key(tileset, tile)
	val, err := this.mc.Get(key)
	if err != nil {
		if err == memcache.ErrCacheMiss {
			log.Printf("load mem err: %s", err)
			err = ErrNoTile
		}
		return
	}
	log.Printf("load mem: %s", key)
	tile.body = val.Value
	return
}

// An HTTP handler which returns a terrain tile resource
func terrainHandler(stores []Storer) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var t Terrain

		// get the tile coordinate from the URL
		vars := mux.Vars(r)
		err := t.parseCoord(vars["x"], vars["y"], vars["z"])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Try and get a tile from the stores
		var idx int
		for i, store := range stores {
			idx = i
			err = store.Load(vars["tileset"], &t)
			if err == nil {
				break
			} else if err == ErrNoTile {
				continue
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		if err == ErrNoTile {
			if t.IsRoot() {
				// serve up a blank tile as it is a missing root tile
				data, err := assets.Asset("data/smallterrain-blank.terrain")
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				} else {
					t.body = data
				}
			} else {
				http.Error(w, errors.New("The terrain tile does not exist").Error(), http.StatusNotFound)
				return
			}
		}

		// send the tile to the client
		headers := w.Header()
		headers.Set("Content-Type", "application/octet-stream")
		headers.Set("Content-Encoding", "gzip")
		headers.Set("Content-Disposition", "attachment;filename="+vars["y"]+".terrain")
		w.Write(t.body)

		// Save the tile in any preceding stores that didn't have it.
		if idx > 0 {
			for j := 0; j < idx; j++ {
				if err := stores[j].Save(vars["tileset"], &t); err != nil {
					log.Printf("failed to store tileset: %s", err)
				}
			}
		}
	}
}

// An HTTP handler which returns a tileset's `layer.json` file
func layerHandler(tilesetRoot string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		filename := filepath.Join(tilesetRoot, vars["tileset"], "layer.json")

		// try and read the `layer.json` from disk
		body, err := ioutil.ReadFile(filename)
		if err != nil {
			if os.IsNotExist(err) {
				// check whether the tile directory exists
				_, err := os.Stat(filepath.Join(tilesetRoot, vars["tileset"]))
				if err != nil {
					if os.IsNotExist(err) {
						http.Error(w,
							fmt.Errorf("The tileset `%s` does not exist", vars["tileset"]).Error(),
							http.StatusNotFound)
						return
					} else {
						// There's some other problem (e.g. permissions)
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
				}

				// the directory exists: send the default `layer.json`
				body = []byte(`{
  "tilejson": "2.1.0",
  "format": "heightmap-1.0",
  "version": "1.0.0",
  "scheme": "tms",
  "tiles": ["{z}/{x}/{y}.terrain"]
}`)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		headers := w.Header()
		headers.Set("Content-Type", "application/json")
		w.Write(body)
	}
}

// Return HTTP middleware which allows CORS requests from any domain
func addCorsHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		headers.Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

func main() {
	port := flag.Uint("port", 8000, "the port on which the server listens")
	tilesetRoot := flag.String("dir", ".", "the root directory under which tileset directories reside")
	memcache := flag.String("memcache", "", "memcache connection string for caching tiles e.g. localhost:11211")
	flag.Parse()

	stores := []Storer{NewFileStore(*tilesetRoot)}

	// If a memcache server has been specified, prepend it to the list of stores.
	if len(*memcache) > 0 {
		stores = append([]Storer{NewMemcacheStore(*memcache)}, stores...)
	}

	r := mux.NewRouter()
	r.HandleFunc("/tilesets/{tileset}/layer.json", layerHandler(*tilesetRoot))
	r.HandleFunc("/tilesets/{tileset}/{z:[0-9]+}/{x:[0-9]+}/{y:[0-9]+}.terrain", terrainHandler(stores))

	http.Handle("/", handlers.CombinedLoggingHandler(os.Stdout, addCorsHeader(r)))

	log.Println("Terrain server listening on port", *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}