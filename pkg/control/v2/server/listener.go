// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

//go:build !windows

package server

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/elastic/elastic-agent/internal/pkg/agent/errors"
	"github.com/elastic/elastic-agent/pkg/control"
	"github.com/elastic/elastic-agent/pkg/core/logger"
	"github.com/elastic/elastic-agent/pkg/utils"
)

func createListener(log *logger.Logger) (net.Listener, error) {
	path := strings.TrimPrefix(control.Address(), "unix://")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		cleanupListener(log)
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		err = os.MkdirAll(dir, 0775)
		if err != nil {
			return nil, err
		}
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0700)
	root, _ := utils.HasRoot() // error ignored
	if !root {
		// allow group access when not running as root
		mode = os.FileMode(0770)
	}
	err = os.Chmod(path, mode)
	if err != nil {
		// failed to set permissions (close listener)
		lis.Close()
		return nil, err
	}
	return lis, err
}

func cleanupListener(log *logger.Logger) {
	path := strings.TrimPrefix(control.Address(), "unix://")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Debug("%s", errors.New(err, fmt.Sprintf("Failed to cleanup %s", path), errors.TypeFilesystem, errors.M("path", path)))
	}
}
