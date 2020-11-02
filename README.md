# Marlin TM Bridge for Irisnet
This contains Modified Tendermint core for acting as bridge between Irisnet Node and multicastTcpBridge (marlin peer) for loading/reading data to/from marlin relay.

## Build Marlin Irisnet TM Bridge
```
cd irisnet_tendermint
make build
cd -
```

## Testing connectivity (bridge)
Start a dummy TCP listener on port 15003
```
python3 simpleTCPlistener.py
```

Initialise the home directory for modified tendermint node. An example directory has been given for reference `irisnet_tmconfig_example`

Run the Marlin Irisnet TM Bridge
```
.irisnet_tendermint/build/tendermint node --proxy_app=kvstore --home irisnet_tmconfig_example
```
