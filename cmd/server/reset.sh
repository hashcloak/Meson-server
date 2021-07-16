# /bin/sh

mkdir -p /tmp/meson_server
read -p "path to katzenmint-pki parent folder? " path
file="$path/katzenmint-pki/docker/conf/node1/data/blockstore.db/000001.log"
hashhex=$(strings $file | grep "CBH:" | head -1 | cut -c5-)
output=""
for idx in $(seq 1 32)
do
	hex=$(echo $hashhex | cut -c-2 | tr [:lower:] [:upper:])
	hashhex=$(echo $hashhex | cut -c3-)
	if [ $idx -gt 1 ]
	then
		output="$output,"
	fi
	output="$output $(echo "ibase=16; $hex" | bc)"
done

TITLE="Hash = "
sed -i "s/$TITLE.*/$TITLE[$output]/g" ./katzenpost.toml.sample
