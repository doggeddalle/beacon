#!/bin/sh
# Ensure the binary and helper scripts are executable after extraction.
PKG_PATH=/usr/local/AppCentral/Beacon
chmod +x "$PKG_PATH/beacon" "$PKG_PATH/beaconctl.sh" 2>/dev/null
exit 0
