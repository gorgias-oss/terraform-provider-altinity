// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

// Command terraform-provider-altinity is the plugin entrypoint. It serves the
// Altinity.Cloud ClickHouse provider over Terraform plugin protocol v6, which
// is understood by both Terraform (>= 1.0, floor 1.5.7) and OpenTofu.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/Gorgias/terraform-provider-altinity/internal/provider"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		// Registry address consumers declare in required_providers.
		Address: "registry.terraform.io/gorgias/altinity",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err.Error())
	}
}
