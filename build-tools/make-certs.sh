#!/bin/bash
#
# make-certs.sh creates the keys and the certificates the api server
# terminates TLS with, along with the client certificate the callers
# authenticate with.  Every key is Ed25519.
#
# A set already in the directory is left alone; -f replaces it.

set -eu

program="${0##*/}"

# days is how long the generated certificates are good for.
days=31

usage() {
	cat <<EOF
Usage: $program [-f] [directory]

Creates a certificate authority, a server certificate issued for localhost,
and a client certificate, in the directory, which defaults to certs.  A set
already there is left alone; -f replaces it.

  -f, --force  replace a set that is already there
EOF
}

force=false

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	-f | --force)
		force=true
		;;
	-*)
		printf '%s: unknown option %s\n' "$program" "$1" >&2
		usage >&2
		exit 2
		;;
	*)
		break
		;;
	esac
	shift
done

dir=${1:-certs}

# The files of a set, which is of no use in part.
outputs="ca.key ca.crt server.key server.crt client.key client.crt"

if [ "$force" = false ]; then
	written=
	missing=
	for output in $outputs; do
		if [ -e "$dir/$output" ]; then
			written="$written $output"
		else
			missing="$missing $output"
		fi
	done

	if [ -n "$written" ] && [ -z "$missing" ]; then
		cat <<EOF
$dir already holds a set, which this leaves alone.  Pass -f to replace it.
EOF
		exit 0
	fi

	if [ -n "$written" ]; then
		cat >&2 <<EOF
$dir holds part of a set:

$written

and not the rest:

$missing

A set is of no use in part, so nothing was replaced.  Remove what is in $dir
and run this again, or pass -f.
EOF
		exit 1
	fi
fi

# Keys are 'secret' so ensure they are readable only for the user.
umask 077

mkdir -p "$dir"

# Location for temporary files.
scratch=$(mktemp -d ./make-certs.XXXXXXXXXXX)
trap 'rm -rf "$scratch"' EXIT

# The certificate authority signs both ends of the connection, so it holds both
# of them back together.  pathlen:0 forbids it from signing another authority,
# which nothing here needs.
openssl genpkey -algorithm ED25519 -out "$dir/ca.key"
openssl req \
	-new \
	-x509 \
	-key "$dir/ca.key" \
	-out "$dir/ca.crt" \
	-days "$days" \
	-subj '/CN=teleport-sre CA/O=teleport-sre' \
	-addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
	-addext 'keyUsage=critical,digitalSignature,keyCertSign,cRLSign'

# The server certificate is issued for the loopback addresses, since the api is
# reached through a gateway on the workstation of the one using it.
cat >"$scratch/server.ext" <<'EOF'
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = DNS:localhost,IP:127.0.0.1
EOF
openssl genpkey \
	-algorithm ED25519 \
	-out "$dir/server.key"
openssl req \
	-new \
	-key "$dir/server.key" \
	-out "$scratch/server.csr" \
	-subj '/CN=localhost/O=teleport-sre'
openssl x509 \
	-req \
	-in "$scratch/server.csr" \
	-CA "$dir/ca.crt" \
	-CAkey "$dir/ca.key" \
	-CAserial "$scratch/ca.srl" \
	-CAcreateserial \
	-out "$dir/server.crt" \
	-days "$days" \
	-extfile "$scratch/server.ext"

# The client certificate carries no name the service reads.  It says the
# caller holds a key the certificate authority gave a certificate for, which
# is all mTLS asks of it.
cat >"$scratch/client.ext" <<'EOF'
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
EOF
openssl genpkey \
	-algorithm ED25519 \
	-out "$dir/client.key"
openssl req \
	-new \
	-key "$dir/client.key" \
	-out "$scratch/client.csr" \
	-subj '/CN=teleport-sre caller/O=teleport-sre'
openssl x509 \
	-req \
	-in "$scratch/client.csr" \
	-CA "$dir/ca.crt" \
	-CAkey "$dir/ca.key" \
	-CAserial "$scratch/ca.srl" \
	-CAcreateserial \
	-out "$dir/client.crt" \
	-days "$days" \
	-extfile "$scratch/client.ext"

openssl verify -CAfile "$dir/ca.crt" "$dir/server.crt" "$dir/client.crt"

cat <<EOF
Wrote to $dir:
  ca.crt      the certificate authority, which the api server verifies the
              caller with, and which the caller verifies the api server with
  ca.key      the private key of the certificate authority
  server.crt  the certificate the api server serves
  server.key  the private key of the server certificate
  client.crt  the certificate a caller authenticates with
  client.key  the private key of the client certificate
EOF

exit 0
