package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"

	"github.com/docker/go-units"
	"github.com/flynn/flynn/controller/client"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/host/types"
	"github.com/flynn/flynn/pkg/dialer"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "USAGE: %s DIR\n", os.Args[0])
		os.Exit(1)
	}

	if err := run(os.Args[1]); err != nil {
		log.Fatalln("ERROR:", "could not create slug artifact:", err)
	}
}

func run(dir string) error {
	client, err := controller.NewClient("", os.Getenv("CONTROLLER_KEY"))
	if err != nil {
		return err
	}

	runnerArtifact, err := client.GetArtifact(os.Getenv("SLUGRUNNER_ARTIFACT_ID"))
	if err != nil {
		return err
	}

	// create a squashfs layer
	layer, err := ioutil.TempFile("", "squashfs-")
	if err != nil {
		return err
	}
	defer os.Remove(layer.Name())
	defer layer.Close()

	if out, err := exec.Command("mksquashfs", dir, layer.Name(), "-noappend").CombinedOutput(); err != nil {
		return fmt.Errorf("mksquashfs error: %s: %s", err, out)
	}

	h := sha512.New()
	length, err := io.Copy(h, layer)
	if err != nil {
		return err
	}
	layerSha := hex.EncodeToString(h.Sum(nil))

	// upload the layer to the blobstore
	if _, err := layer.Seek(0, os.SEEK_SET); err != nil {
		return err
	}
	layerURL := fmt.Sprintf("http://blobstore.discoverd/slugs/layers/%s.squashfs", layerSha)
	if err := upload(layer, layerURL); err != nil {
		return err
	}

	manifest := &ct.ImageManifest{
		Type: ct.ImageManifestTypeV1,

		// TODO: parse Procfile / .release and add to manifest.Entrypoints
		Entrypoints: map[string]*ct.ImageEntrypoint{
			"_default": {
				Env: map[string]string{
					"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
					"TERM": "xterm",
					"HOME": "/app",
				},
				WorkingDir: "/app",
				Args:       []string{"/runner/init", "bash"},
			},
		},

		Rootfs: []*ct.ImageRootfs{{
			Platform: ct.DefaultImagePlatform,
			Layers: append(runnerArtifact.Manifest.Rootfs[0].Layers, &ct.ImageLayer{
				Type:   ct.ImageLayerTypeSquashfs,
				Length: length,
				Hashes: map[string]string{"sha512": layerSha},
				URL:    layerURL,
			}),
		}},
	}

	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	manifestHash := sha512.Sum512(manifestData)
	manifestURL := fmt.Sprintf("http://blobstore.discoverd/slugs/images/%s.json", hex.EncodeToString(manifestHash[:]))

	if err := upload(bytes.NewReader(manifestData), manifestURL); err != nil {
		return err
	}

	artifact := &ct.Artifact{
		ID:   os.Getenv("SLUG_ARTIFACT_ID"),
		Type: host.ArtifactTypeFlynn,
		URI:  manifestURL,
		Meta: map[string]string{
			"blobstore": "true",
		},
		Manifest: manifest,
	}

	// create artifact
	if err := client.CreateArtifact(artifact); err != nil {
		return err
	}

	fmt.Printf("-----> Compiled slug size is %s\n", units.BytesSize(float64(length)))
	return nil
}

func upload(data io.Reader, url string) error {
	req, err := http.NewRequest("PUT", url, data)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Dial: dialer.Retry.Dial}}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	return nil
}
