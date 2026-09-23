#!/bin/sh
# Build xrok locally (never on the server) and install/upgrade it as a
# bare-metal systemd service on the target host.
#
#   deploy/install.sh ubuntu@52.77.122.131 ~/Documents/Singa-btfs.pem
#
# First install generates an API token on the server and prints it once.
set -eu
HOST=${1:?usage: install.sh user@host [ssh-key]}
KEY=${2:-}
SSH="ssh ${KEY:+-i $KEY}"
SCP="scp -q ${KEY:+-i $KEY}"
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

VERSION=$(git -C "$ROOT" describe --always --dirty)
echo "building linux/amd64 $VERSION"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -trimpath -ldflags "-s -w" -o "$OUT/xrok" .)

COPYFILE_DISABLE=1 tar --no-xattrs -C "$ROOT/deploy" -cf "$OUT/deploy.tar" .
$SCP "$OUT/xrok" "$OUT/deploy.tar" "$HOST:/tmp/"

$SSH "$HOST" sudo sh -s <<'REMOTE'
set -eu
cd /tmp && rm -rf xrok-deploy && mkdir xrok-deploy && tar -C xrok-deploy -xf deploy.tar
D=/tmp/xrok-deploy

id xrok >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin xrok
install -d -m 0755 /opt/xrok
install -d -m 0750 -o root -g xrok /etc/xrok
install -d -m 0750 -o xrok -g xrok /var/lib/xrok /var/log/xrok

install -m 0755 /tmp/xrok /opt/xrok/xrok.new && mv -f /opt/xrok/xrok.new /opt/xrok/xrok
install -m 0755 $D/xrok-egress-fw $D/xrok-cert-sync /usr/local/sbin/
install -m 0644 $D/xrok.service $D/xrok-cert-sync.service $D/xrok-cert-sync.timer /etc/systemd/system/
install -m 0644 $D/xrok.logrotate /etc/logrotate.d/xrok

if [ ! -f /etc/xrok/server.yaml ]; then
	TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | cut -c1-40)
	sed "s/__TOKEN__/$TOKEN/" $D/server.yaml.example > /etc/xrok/server.yaml
	chown root:xrok /etc/xrok/server.yaml && chmod 0640 /etc/xrok/server.yaml
	echo "generated api_token in /etc/xrok/server.yaml"
fi

/usr/local/sbin/xrok-cert-sync
systemctl daemon-reload
systemctl enable --now xrok-cert-sync.timer
systemctl enable xrok
systemctl restart xrok
sleep 2
systemctl --no-pager --lines=0 status xrok | head -5
rm -rf /tmp/xrok /tmp/deploy.tar $D
REMOTE
