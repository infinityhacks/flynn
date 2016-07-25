package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/flynn/flynn/pinkerton"
	"github.com/flynn/flynn/pkg/imagebuilder"
)

func main() {
	log.SetFlags(0)

	if len(os.Args) != 2 {
		log.Fatalf("usage: %s NAME", os.Args[0])
	}
	if err := build(os.Args[1]); err != nil {
		log.Fatalln("ERROR:", err)
	}
}

func build(name string) error {
	cmd := exec.Command("docker", "build", "-t", name, ".")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error building docker image: %s", err)
	}

	context, err := pinkerton.BuildContext("aufs", "/var/lib/docker")
	if err != nil {
		return err
	}

	layerDir := "/var/lib/flynn/layer-cache"
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return err
	}

	b := &imagebuilder.Builder{
		LayerDir: layerDir,
		Context:  context,
	}

	manifest, err := b.Build(name, true)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(manifest)
}
