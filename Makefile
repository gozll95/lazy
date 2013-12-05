# Copyright 2012, guxianje. All rights reserved.
# Use of this source code is governed by a BSD-style license that can
# be found in the LICENSE file.

MAKEFLAGS = -s
all:
	go build analyzer.go worker.go config.go
	go build learner.go worker.go config.go

analyzer:
	go build analyzer.go worker.go config.go 

learner:
	go build learner.go worker.go config.go 

fmt:
	go fmt

lint:
	golint *.go

clean:
	go clean