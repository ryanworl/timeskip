#!/bin/sh
# Contend for the lock for ten minutes; other clients run alongside.
exec client -for 10m
