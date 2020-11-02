import socket

s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 15003))
s.listen(1)

print("Started TCP test server")
while True:
    # now our endpoint knows about the OTHER endpoint.
    clientsocket, address = s.accept()
    print(f"Connection from {address} has been established.")
    with clientsocket:
        while True:
            data = clientsocket.recv(1024)
            if not data:
                continue
            print("DATA: " + str(data))
