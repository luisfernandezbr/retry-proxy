#
# Makefile for building all things related to this repo
#
NAME := pipeline
ORG := retry-proxy
PKG := $(ORG)/$(NAME)
SHELL := /bin/bash

docker:
	@docker build . -t $(PKG)

publish:
	@docker push $(PKG)
