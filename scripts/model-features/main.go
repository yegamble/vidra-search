// Command model-features exposes the serving feature order to the training test.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/vidra/vidra-search/internal/ranking"
)

func main() {
	if err := json.NewEncoder(os.Stdout).Encode(ranking.ModelFeatureNames()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
