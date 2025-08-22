#!/bin/sh

/usr/local/bin/gobgpd &
/usr/local/bin/manager
wait
