# xrok

## Features
- **Local Application Proxying**: Redirect traffic from a remote server to a local application on your computer.
- **Publishing Local Files and Directories**: Provide access to your local files or entire directories via a remote server.
- **TLS Support**: Securely encrypt connections between the client and server using TLS.
- **Local HTTP Proxy**: Option to use a local proxy server to handle HTTP requests.
- **Flexible Port Configuration**: Automatically select free ports within a specified range to ensure proper operation.

## Requirements
- **Go**: Version 1.16 or higher to build the source code.
- **TLS Certificates**: Required for working with secure connections (TLS).
- **Domain Name**: An active domain name is necessary for server setup.

## Installation

### Building from Source
1. Install Go by following the instructions on the [official website](https://golang.org/doc/install).
2. Clone the project repository:
   ```bash
   git clone https://github.com/imhassla/xrok.git
   ```
3. Navigate to the project directory and build the binaries:
   ```bash
   cd xrok
   go mod init xrok
   go mod tidy
   go build -o server server.go
   go build -o client client.go
   ```

### Server Setup

#### Generating and Configuring TLS Certificates
To work with TLS, you need to generate SSL certificates for your domain. You can use Let's Encrypt or another certificate authority.

Example using Let's Encrypt with Certbot:
```bash
sudo apt-get install certbot
sudo certbot certonly --standalone -d yourdomain.com
sudo certbot certonly --manual --preferred-challenges=dns -d "yourdomain.com, *.yourdomain.com"
```
Certificates will be saved in `/etc/letsencrypt/live/yourdomain.com/`.

#### Running the Server

Start the server by specifying the domain:
```bash
sudo ./server -domain yourdomain.com
```

**Command-Line Options for the Server**:
- `-domain` (required): The domain name to be used in URLs when registering clients.
- `-debug` (optional): Enables debug logging.
- `-rp` (optional): Port for client registration (default is 7645).

### Client Setup

#### Publishing a Local Application
If you want to proxy a local application:
1. Run your local application on a specific port, for example, `localhost:8080`.
2. Start the client by specifying the server and the local application's port:
   ```bash
   ./client --server yourdomain.com --port localhost:8080
   ```
   After successful registration, the client will display a Proxy URL where your application will be accessible from the internet.

#### Publishing a Local Directory
If you want to provide access to a local directory:
1. Ensure that the directory exists and you have read permissions.
2. Start the client by specifying the server and the path to the directory:
   ```bash
   ./client --server yourdomain.com --folder /path/to/your/folder
   ```
   The client will start a local file server and provide a URL to access the files in the specified directory.

#### Using the Local HTTP Proxy
If you want to use a local proxy to handle HTTP requests:
```bash
./client --server yourdomain.com --proxy
```
The client will set up a local HTTP proxy server. You can configure your applications to use this proxy to handle HTTP requests through the remote server.

**Command-Line Options for the Client**:
- `--server` (required): The server address to connect to.
- `--port`: The address and port of the local application to proxy (e.g., `localhost:8080`).
- `--folder`: The path to the local directory to publish.
- `--proxy`: Use a local HTTP proxy to handle requests.
- `--tls`: Use TLS for connections (default is true).
- `--id`: Use custom id(subdomain) for TLS proxy URL.
- `--debug`: Enable debug logging.
- `--rp`: The port for client registration on the server (must match the `-rp` port on the server, default is 7645).

**Note**: The options `--port`, `--folder`, and `--proxy` are mutually exclusive. Use only one of them when starting the client.

## Applications
- **Web Application Development and Testing**: Quickly provide access to a local application for colleagues or clients without deploying it to hosting.
- **File Sharing**: A convenient way to share files or directories located on your local computer.
- **Secure Connection**: Using TLS ensures data encryption between the client and server.
- **Remote Access**: Allows you to access local resources from anywhere in the world.

## Debugging and Troubleshooting
- **Connection Issues**: Ensure there are no network restrictions or blocks between the client and server.
- **Ports in Use**: If the application cannot occupy the required port, ensure it is not used by another application or change the port range in the code.
- **TLS Certificates**: Verify that certificates are correctly installed and paths to them are specified correctly.
- **Debugging**: Use the `--debug` flag to output additional information to assist in diagnosing problems.

## Contributing
We welcome community contributions to enhance this project. If you have suggestions or have found a bug, please create an issue or submit a pull request.

## License
This project is distributed under the MIT License. See the LICENSE file in the root directory of the project for details.

## Conclusion
This project provides a simple and effective way to grant access to local applications and files through a remote server. With TLS support and flexible configuration, it is suitable for various use cases, from development to file sharing. Follow the instructions in this documentation for quick setup and to get started.

