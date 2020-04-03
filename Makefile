#
# Makefile for building all things related to this repo
#
NAME := retry-proxy
ORG := pinpt
PKG := $(ORG)/$(NAME)
SHELL := /bin/bash
TAG ?= latest

docker:
	@docker build . -t $(PKG)

publish:
	@docker tag $(PKG) $(PKG):$(TAG)
	@docker push $(PKG):$(TAG)
