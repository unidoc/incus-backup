package main

import "github.com/unidoc/incus-backup/internal/client/config"

func configFor(path string) (*config.Config, error) { return config.Load(path) }
