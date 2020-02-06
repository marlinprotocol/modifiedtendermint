# Marlin <> TM
---

## Participants
- r_tm - Real TM node
- m_tm - Modified TM node
- m_sdk - MarlinMulticastSDK
- m_bridge - TCP Bridge between modified TM node and MarlinMulticastSDK

## Modified TM node changes

### Code

#### Reactors
- Blockchain reactor
	- fast-sync turned off to prevent the m_tm node from trying to sync up with mainchain and move straight forward to consensus mode

- Consensus reactor
	- reflecting back r_tm node's newroundstep message
 	- receive from m_bridge -> send to r_tm node
	- receive from r_tm node -> send to m_brige via marlinPeer
	- TODO: consensus reactor early return from receive (after reflecting the NRS message
- Mempool reactor
	- receive from m_bridge -> send to r_tm node
	- receive from r_tm node -> send to m_brige via marlinPeer

- PEX reactor
	- turned off via a config change [p2p] pex = false

- TODO: look into failure and retry when tcp bridge not running
- TODO: failure on update round

#### Marlin Peer
- implements Peer interface
- connects m_tm to m_bridge using a tcp connection
- lpf message format
	- 8 byte, message length = chID length (1 byte) + actual_message_length
	- 1 byte, chID
	- actual message (arbitrary bytes)

### Config
- [main base config options] fast_sync = false
- [p2p] pex = false
- [p2p] persistent_peers = "r_tm_ID@ip:port"
- [p2p] marlin_peer = "0000000000000000000000000000000000000000@m_bridge_address"

### Command line
- \-\- fast_sync false
- \-\- p2p.pex false
- \-\- p2p.persistent_peers "r_tm_ID@ip:port"
- \-\- p2p.marlin_peer "0000000000000000000000000000000000000000@m_bridge_address"
