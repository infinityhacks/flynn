package imagebuilder

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/docker/docker/pkg/archive"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/pinkerton"
)

type Builder struct {
	LayerDir string
	Context  *pinkerton.Context
}

func (b *Builder) Build(name string, byTags bool) (*ct.ImageManifest, error) {
	image, err := b.Context.LookupImage(name)
	if err != nil {
		return nil, err
	}

	history, err := b.Context.History(name)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(history))
	layers := make([]*ct.ImageLayer, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		layer := history[i]
		ids = append(ids, layer.ID)
		if !byTags || len(layer.Tags) > 0 {
			l, err := b.CreateLayer(ids)
			if err != nil {
				return nil, err
			}
			ids = make([]string, 0, len(history))
			layers = append(layers, l)
		}
	}

	entrypoint := &ct.ImageEntrypoint{
		WorkingDir: image.Config.WorkingDir,
		Env:        make(map[string]string, len(image.Config.Env)),
		Args:       append(image.Config.Entrypoint.Slice(), image.Config.Cmd.Slice()...),
	}
	for _, env := range image.Config.Env {
		keyVal := strings.SplitN(env, "=", 2)
		if len(keyVal) != 2 {
			continue
		}
		entrypoint.Env[keyVal[0]] = keyVal[1]
	}

	return &ct.ImageManifest{
		Type:        ct.ImageManifestTypeV1,
		Entrypoints: map[string]*ct.ImageEntrypoint{"_default": entrypoint},
		Rootfs: []*ct.ImageRootfs{{
			Platform: ct.DefaultImagePlatform,
			Layers:   layers,
		}},
	}, nil
}

// CreateLayer creates a squashfs layer from a docker layer ID chain by
// creating a temporary directory, applying the relevant diffs then calling
// mksquashfs.
//
// Each squashfs layer is serialized as JSON and cached in a temporary file to
// avoid regenerating existing layers, with access wrapped with a lock file in
// case multiple images are being built at the same time.
func (b *Builder) CreateLayer(ids []string) (*ct.ImageLayer, error) {
	imageID := ids[len(ids)-1]
	layerJSON := filepath.Join(b.LayerDir, imageID+".json")

	// acquire the lock file using flock(2) to synchronize access to the
	// layer JSON
	lockPath := layerJSON + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	defer os.Remove(lock.Name())
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	// if the layer JSON exists, deserialize and return
	f, err := os.Open(layerJSON)
	if err == nil {
		defer f.Close()
		var layer ct.ImageLayer
		return &layer, json.NewDecoder(f).Decode(&layer)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	// apply the docker layer diffs to a temporary directory
	dir, err := ioutil.TempDir("", "docker-layer-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	for _, id := range ids {
		// TODO: AUFS whiteouts
		diff, err := b.Context.Diff(id, "")
		if err != nil {
			return nil, err
		}
		if err := archive.Untar(diff, dir, &archive.TarOptions{}); err != nil {
			return nil, err
		}
	}

	// create the squashfs layer
	layer, err := b.mksquashfs(dir)
	if err != nil {
		return nil, err
	}

	// write the serialized layer to the JSON file
	f, err = os.Create(layerJSON)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(&layer); err != nil {
		os.Remove(layerJSON)
		return nil, err
	}
	return layer, nil
}

func (b *Builder) mksquashfs(dir string) (*ct.ImageLayer, error) {
	tmp, err := ioutil.TempFile("", "squashfs-")
	if err != nil {
		return nil, err
	}
	defer tmp.Close()

	if out, err := exec.Command("mksquashfs", dir, tmp.Name(), "-noappend").CombinedOutput(); err != nil {
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("mksquashfs error: %s: %s", err, out)
	}

	h := sha512.New()
	length, err := io.Copy(h, tmp)
	if err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}

	sha512 := hex.EncodeToString(h.Sum(nil))
	dst := filepath.Join(b.LayerDir, sha512+".squashfs")
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return nil, err
	}

	return &ct.ImageLayer{
		Type:   ct.ImageLayerTypeSquashfs,
		Length: length,
		Hashes: map[string]string{"sha512": sha512},
	}, nil
}
