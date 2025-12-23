#!/bin/bash
#
# xrok Server Setup Script
# Installs all dependencies, builds, validates, and configures xrok server
#
# Usage: ./setup-server.sh -d xrok.example.com
#

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

# Default values
DOMAIN=""
REPO_DIR=""
BINARY_PATH=""
SERVICE_NAME="xrok"
DEBUG_MODE="false"
REQUEST_LOG="false"
GO_VERSION="1.22.0"
NON_INTERACTIVE="false"
SKIP_CERT="false"
AUTO_START="false"

# Print functions
info() { echo -e "${BLUE}[INFO]${NC} $1"; }
success() { echo -e "${GREEN}[OK]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }
step() { echo -e "\n${CYAN}==>${NC} ${GREEN}$1${NC}"; }
ask() { echo -en "${YELLOW}[?]${NC} $1 "; }

usage() {
    cat << EOF
Usage: $(basename "$0") [OPTIONS]

xrok Server Setup Script - Full installation

OPTIONS:
    -d, --domain DOMAIN     Domain name (required)
    -r, --repo PATH         Path to xrok repository (default: ~/xrok)
    -D, --debug             Enable debug mode
    -L, --request-log       Enable request logging
    -y, --yes               Non-interactive mode (answer yes to all)
    --skip-cert             Skip certificate setup (use existing)
    --auto-start            Automatically start and enable service
    -h, --help              Show this help

EXAMPLES:
    $(basename "$0") -d xrok.example.com
    $(basename "$0") -d xrok.example.com -r /opt/xrok --debug
    $(basename "$0") -d xrok.example.com -y --skip-cert --auto-start

EOF
    exit 0
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -d|--domain) DOMAIN="$2"; shift 2 ;;
        -r|--repo) REPO_DIR="$2"; shift 2 ;;
        -D|--debug) DEBUG_MODE="true"; shift ;;
        -L|--request-log) REQUEST_LOG="true"; shift ;;
        -y|--yes) NON_INTERACTIVE="true"; shift ;;
        --skip-cert) SKIP_CERT="true"; shift ;;
        --auto-start) AUTO_START="true"; shift ;;
        -h|--help) usage ;;
        *) error "Unknown option: $1" ;;
    esac
done

[[ -z "$DOMAIN" ]] && error "Domain is required. Use -d or --domain"
[[ -z "$REPO_DIR" ]] && REPO_DIR="$HOME/xrok"

# Detect OS
detect_os() {
    if [[ -f /etc/os-release ]]; then
        . /etc/os-release
        OS=$ID
        OS_VERSION=$VERSION_ID
    else
        error "Cannot detect OS"
    fi
}

#######################################
# Step 1: Install system dependencies
#######################################
install_dependencies() {
    step "Installing system dependencies"

    detect_os
    info "Detected OS: $OS $OS_VERSION"

    case $OS in
        ubuntu|debian)
            sudo apt-get update
            sudo apt-get install -y \
                curl wget git \
                build-essential \
                dnsutils \
                certbot \
                python3-certbot-dns-route53 \
                python3-certbot-dns-cloudflare \
                openssl \
                jq
            success "Dependencies installed"
            ;;
        centos|rhel|fedora|rocky|alma)
            sudo yum install -y epel-release || true
            sudo yum install -y \
                curl wget git \
                gcc make \
                bind-utils \
                certbot \
                python3-certbot-dns-route53 \
                python3-certbot-dns-cloudflare \
                openssl \
                jq
            success "Dependencies installed"
            ;;
        *)
            warn "Unknown OS: $OS. Trying to continue..."
            ;;
    esac
}

#######################################
# Step 2: Install Go
#######################################
install_go() {
    step "Checking Go installation"

    if command -v go &> /dev/null; then
        CURRENT_GO=$(go version | grep -oP 'go\d+\.\d+' | head -1)
        success "Go already installed: $(go version)"
        return 0
    fi

    info "Installing Go $GO_VERSION..."

    # Detect architecture
    ARCH=$(uname -m)
    case $ARCH in
        x86_64) GO_ARCH="amd64" ;;
        aarch64|arm64) GO_ARCH="arm64" ;;
        armv7l) GO_ARCH="armv6l" ;;
        *) error "Unsupported architecture: $ARCH" ;;
    esac

    GO_TAR="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
    GO_URL="https://go.dev/dl/${GO_TAR}"

    info "Downloading $GO_URL..."
    cd /tmp
    wget -q "$GO_URL" || curl -sLO "$GO_URL"

    info "Installing to /usr/local/go..."
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$GO_TAR"
    rm -f "$GO_TAR"

    # Add to PATH
    if ! grep -q '/usr/local/go/bin' ~/.bashrc; then
        echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
    fi
    if ! grep -q '/usr/local/go/bin' /etc/profile.d/go.sh 2>/dev/null; then
        echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/go.sh > /dev/null
    fi

    export PATH=$PATH:/usr/local/go/bin

    if command -v go &> /dev/null; then
        success "Go installed: $(go version)"
    else
        error "Go installation failed"
    fi
}

#######################################
# Step 3: Clone/update repository
#######################################
setup_repository() {
    step "Setting up xrok repository"

    if [[ -d "$REPO_DIR/.git" ]]; then
        info "Repository exists at $REPO_DIR"
        cd "$REPO_DIR"

        if [[ "$NON_INTERACTIVE" == "true" ]]; then
            info "Updating repository..."
            git pull || warn "Could not pull updates"
        else
            ask "Update repository? (y/n)"
            read -r UPDATE_REPO
            if [[ "$UPDATE_REPO" == "y" ]]; then
                git pull || warn "Could not pull updates"
            fi
        fi
    elif [[ -d "$REPO_DIR" ]]; then
        info "Directory exists but not a git repo: $REPO_DIR"
        info "Using existing files"
    else
        info "Cloning xrok repository..."
        git clone https://github.com/imhassla/xrok.git "$REPO_DIR" || error "Failed to clone repository"
    fi

    cd "$REPO_DIR"
    success "Repository ready at $REPO_DIR"
}

#######################################
# Step 4: Build xrok
#######################################
build_xrok() {
    step "Building xrok"

    cd "$REPO_DIR"
    export PATH=$PATH:/usr/local/go/bin

    info "Running go build..."
    go build -o xrok . || error "Build failed"

    BINARY_PATH="$REPO_DIR/xrok"
    chmod +x "$BINARY_PATH"

    # Verify
    if "$BINARY_PATH" --help &> /dev/null; then
        success "Build successful: $BINARY_PATH"
        "$BINARY_PATH" --help | head -5
    else
        error "Binary verification failed"
    fi
}

#######################################
# Step 5: Check DNS
#######################################
check_dns() {
    step "Checking DNS configuration"

    # Get server IP
    info "Detecting server public IP..."
    SERVER_IP=$(curl -s --max-time 5 ifconfig.me 2>/dev/null || \
                curl -s --max-time 5 icanhazip.com 2>/dev/null || \
                curl -s --max-time 5 ipecho.net/plain 2>/dev/null)

    if [[ -z "$SERVER_IP" ]]; then
        warn "Could not detect public IP"
        SERVER_IP="UNKNOWN"
    else
        success "Server public IP: $SERVER_IP"
    fi

    # Check main domain
    info "Checking DNS for $DOMAIN..."
    DOMAIN_IP=$(dig +short "$DOMAIN" A 2>/dev/null | head -1)

    if [[ -z "$DOMAIN_IP" ]]; then
        warn "Domain $DOMAIN does not resolve"
        echo ""
        echo "  Add DNS A record:"
        echo "    $DOMAIN -> $SERVER_IP"
        DNS_OK=false
    elif [[ "$DOMAIN_IP" == "$SERVER_IP" ]]; then
        success "Domain $DOMAIN -> $SERVER_IP (correct)"
        DNS_OK=true
    else
        warn "Domain points to $DOMAIN_IP (expected $SERVER_IP)"
        DNS_OK=false
    fi

    # Check wildcard
    info "Checking wildcard DNS *.$DOMAIN..."
    WILDCARD_TEST="xrok-dns-test-$(date +%s).$DOMAIN"
    WILDCARD_IP=$(dig +short "$WILDCARD_TEST" A 2>/dev/null | head -1)

    if [[ -z "$WILDCARD_IP" ]]; then
        warn "Wildcard DNS not configured"
        echo ""
        echo "  Add DNS A record for subdomain routing:"
        echo "    *.$DOMAIN -> $SERVER_IP"
    elif [[ "$WILDCARD_IP" == "$SERVER_IP" ]]; then
        success "Wildcard *.$DOMAIN -> $SERVER_IP (correct)"
    else
        warn "Wildcard points to $WILDCARD_IP (expected $SERVER_IP)"
    fi

    if [[ "$DNS_OK" != "true" ]]; then
        if [[ "$NON_INTERACTIVE" == "true" ]]; then
            warn "DNS not correctly configured, continuing anyway..."
        else
            echo ""
            ask "Continue without correct DNS? (y/n)"
            read -r CONTINUE_DNS
            [[ "$CONTINUE_DNS" != "y" ]] && error "Fix DNS and re-run script"
        fi
    fi
}

#######################################
# Step 6: Setup TLS certificates
#######################################
setup_certificates() {
    step "Setting up TLS certificates"

    # Skip if requested
    if [[ "$SKIP_CERT" == "true" ]]; then
        info "Skipping certificate setup (--skip-cert)"
        return 0
    fi

    CERT_DIR="/etc/letsencrypt/live/$DOMAIN"
    CERT_PATH="$CERT_DIR/fullchain.pem"
    KEY_PATH="$CERT_DIR/privkey.pem"

    # Check existing certificates
    if sudo test -f "$CERT_PATH" && sudo test -f "$KEY_PATH"; then
        info "Found existing certificates"

        # Check validity
        if sudo openssl x509 -in "$CERT_PATH" -noout -checkend 604800 2>/dev/null; then
            EXPIRY=$(sudo openssl x509 -in "$CERT_PATH" -noout -enddate | cut -d= -f2)
            success "Certificate valid until: $EXPIRY"

            # Check if wildcard
            CERT_DOMAINS=$(sudo openssl x509 -in "$CERT_PATH" -noout -text 2>/dev/null | grep -A1 "Subject Alternative Name" | tail -1)
            if [[ "$CERT_DOMAINS" == *"*.$DOMAIN"* ]]; then
                success "Wildcard certificate detected"
            else
                warn "Not a wildcard certificate - subdomain routing limited"
            fi

            return 0
        else
            warn "Certificate expires soon or is invalid"
        fi
    else
        info "No certificates found for $DOMAIN"
    fi

    # In non-interactive mode, try standalone cert
    if [[ "$NON_INTERACTIVE" == "true" ]]; then
        info "Non-interactive mode: attempting standalone certificate..."
        obtain_cert_standalone
        return $?
    fi

    # Offer to obtain certificates
    echo ""
    echo "Certificate options:"
    echo "  1) Single domain certificate ($DOMAIN)"
    echo "  2) Wildcard certificate ($DOMAIN + *.$DOMAIN) - requires DNS plugin"
    echo "  3) Skip certificate setup"
    echo ""
    ask "Choose option (1/2/3):"
    read -r CERT_OPTION

    case $CERT_OPTION in
        1)
            obtain_cert_standalone
            ;;
        2)
            obtain_cert_wildcard
            ;;
        3)
            warn "Skipping certificate setup"
            warn "You'll need to obtain certificates manually before starting the server"
            return 1
            ;;
        *)
            error "Invalid option"
            ;;
    esac
}

obtain_cert_standalone() {
    step "Obtaining single domain certificate"

    # Check if port 80 is free
    if ss -tlnp 2>/dev/null | grep -q ':80 '; then
        warn "Port 80 is in use. Attempting to free it..."
        sudo systemctl stop nginx 2>/dev/null || true
        sudo systemctl stop apache2 2>/dev/null || true
        sudo systemctl stop httpd 2>/dev/null || true
        sleep 2
    fi

    info "Running certbot standalone..."
    sudo certbot certonly --standalone \
        -d "$DOMAIN" \
        --agree-tos \
        --non-interactive \
        --register-unsafely-without-email \
        || error "Failed to obtain certificate"

    success "Certificate obtained for $DOMAIN"
}

obtain_cert_wildcard() {
    step "Obtaining wildcard certificate"

    echo ""
    echo "DNS providers for automatic verification:"
    echo "  1) Cloudflare"
    echo "  2) Route53 (AWS)"
    echo "  3) Manual DNS challenge"
    echo ""
    ask "Choose DNS provider (1/2/3):"
    read -r DNS_PROVIDER

    case $DNS_PROVIDER in
        1)
            obtain_cert_cloudflare
            ;;
        2)
            obtain_cert_route53
            ;;
        3)
            obtain_cert_manual
            ;;
        *)
            error "Invalid option"
            ;;
    esac
}

obtain_cert_cloudflare() {
    info "Setting up Cloudflare DNS plugin"

    echo ""
    ask "Enter Cloudflare API token:"
    read -rs CF_TOKEN
    echo ""

    # Create credentials file
    sudo mkdir -p /etc/letsencrypt
    echo "dns_cloudflare_api_token = $CF_TOKEN" | sudo tee /etc/letsencrypt/cloudflare.ini > /dev/null
    sudo chmod 600 /etc/letsencrypt/cloudflare.ini

    info "Running certbot with Cloudflare DNS..."
    sudo certbot certonly \
        --dns-cloudflare \
        --dns-cloudflare-credentials /etc/letsencrypt/cloudflare.ini \
        -d "$DOMAIN" \
        -d "*.$DOMAIN" \
        --agree-tos \
        --non-interactive \
        --register-unsafely-without-email \
        || error "Failed to obtain certificate"

    success "Wildcard certificate obtained"
}

obtain_cert_route53() {
    info "Setting up Route53 DNS plugin"

    echo ""
    echo "Make sure AWS credentials are configured:"
    echo "  ~/.aws/credentials or environment variables AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY"
    echo ""
    ask "Continue? (y/n)"
    read -r CONTINUE_R53
    [[ "$CONTINUE_R53" != "y" ]] && error "Aborted"

    info "Running certbot with Route53 DNS..."
    sudo certbot certonly \
        --dns-route53 \
        -d "$DOMAIN" \
        -d "*.$DOMAIN" \
        --agree-tos \
        --non-interactive \
        --register-unsafely-without-email \
        || error "Failed to obtain certificate"

    success "Wildcard certificate obtained"
}

obtain_cert_manual() {
    info "Manual DNS challenge"

    echo ""
    warn "You will need to manually add TXT records to your DNS"
    echo ""

    sudo certbot certonly \
        --manual \
        --preferred-challenges dns \
        -d "$DOMAIN" \
        -d "*.$DOMAIN" \
        --agree-tos \
        --register-unsafely-without-email \
        || error "Failed to obtain certificate"

    success "Wildcard certificate obtained"
}

#######################################
# Step 7: Create systemd service
#######################################
create_service() {
    step "Creating systemd service"

    # Build command arguments
    XROK_ARGS="server -domain $DOMAIN"
    [[ "$DEBUG_MODE" == "true" ]] && XROK_ARGS="$XROK_ARGS -debug"
    [[ "$REQUEST_LOG" == "true" ]] && XROK_ARGS="$XROK_ARGS -request-log"

    SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

    cat << EOF | sudo tee "$SERVICE_FILE" > /dev/null
[Unit]
Description=xrok Tunnel Server
Documentation=https://github.com/imhassla/xrok
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
WorkingDirectory=$REPO_DIR
ExecStart=$BINARY_PATH $XROK_ARGS
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=xrok

# Security
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/etc/letsencrypt /var/log $REPO_DIR

# Limits
LimitNOFILE=65535
LimitNPROC=4096

[Install]
WantedBy=multi-user.target
EOF

    success "Service file created: $SERVICE_FILE"

    # Reload systemd
    sudo systemctl daemon-reload
    success "Systemd configuration reloaded"
}

#######################################
# Step 8: Configure firewall
#######################################
configure_firewall() {
    step "Configuring firewall"

    if command -v ufw &> /dev/null; then
        info "Configuring UFW..."
        sudo ufw allow 443/tcp comment 'xrok HTTPS' || true
        sudo ufw allow 80/tcp comment 'xrok HTTP/certbot' || true
        success "UFW rules added"
    elif command -v firewall-cmd &> /dev/null; then
        info "Configuring firewalld..."
        sudo firewall-cmd --permanent --add-service=https || true
        sudo firewall-cmd --permanent --add-service=http || true
        sudo firewall-cmd --reload || true
        success "Firewalld rules added"
    else
        info "No firewall detected (or using cloud security groups)"
    fi
}

#######################################
# Step 9: Setup certbot renewal
#######################################
setup_cert_renewal() {
    step "Setting up certificate auto-renewal"

    # Create renewal hook to reload xrok
    HOOK_DIR="/etc/letsencrypt/renewal-hooks/deploy"
    sudo mkdir -p "$HOOK_DIR"

    cat << 'EOF' | sudo tee "$HOOK_DIR/xrok-reload.sh" > /dev/null
#!/bin/bash
# Reload xrok after certificate renewal
systemctl reload xrok 2>/dev/null || systemctl restart xrok
EOF

    sudo chmod +x "$HOOK_DIR/xrok-reload.sh"
    success "Certificate renewal hook created"

    # Test renewal (dry-run)
    info "Testing certificate renewal..."
    sudo certbot renew --dry-run 2>/dev/null && success "Renewal test passed" || warn "Renewal test failed"
}

#######################################
# Step 10: Final summary
#######################################
show_summary() {
    step "Setup Complete!"

    echo ""
    echo "┌─────────────────────────────────────────────────────┐"
    echo "│              xrok Server Configuration              │"
    echo "├─────────────────────────────────────────────────────┤"
    printf "│  %-15s %-35s │\n" "Domain:" "$DOMAIN"
    printf "│  %-15s %-35s │\n" "Repository:" "$REPO_DIR"
    printf "│  %-15s %-35s │\n" "Binary:" "$BINARY_PATH"
    printf "│  %-15s %-35s │\n" "Service:" "$SERVICE_NAME"
    printf "│  %-15s %-35s │\n" "Debug:" "$DEBUG_MODE"
    echo "└─────────────────────────────────────────────────────┘"
    echo ""
    echo "Service commands:"
    echo "  sudo systemctl start $SERVICE_NAME    # Start server"
    echo "  sudo systemctl stop $SERVICE_NAME     # Stop server"
    echo "  sudo systemctl status $SERVICE_NAME   # Check status"
    echo "  sudo systemctl enable $SERVICE_NAME   # Enable on boot"
    echo "  sudo journalctl -u $SERVICE_NAME -f   # View logs"
    echo ""
    echo "Update xrok:"
    echo "  cd $REPO_DIR && git pull && go build -o xrok ."
    echo "  sudo systemctl restart $SERVICE_NAME"
    echo ""
    echo "Health check:"
    echo "  curl -sk https://$DOMAIN/health"
    echo ""

    # Determine if we should start
    START_NOW="n"
    if [[ "$AUTO_START" == "true" ]] || [[ "$NON_INTERACTIVE" == "true" ]]; then
        START_NOW="y"
    else
        ask "Start xrok service now? (y/n)"
        read -r START_NOW
    fi

    if [[ "$START_NOW" == "y" ]]; then
        # Stop any existing xrok process
        sudo pkill -9 -f "xrok server" 2>/dev/null || true
        sleep 1

        sudo systemctl start "$SERVICE_NAME"
        sleep 3

        if systemctl is-active --quiet "$SERVICE_NAME"; then
            success "Service started!"
            echo ""

            # Test health endpoint
            HEALTH=$(curl -sk "https://$DOMAIN/health" 2>/dev/null)
            if [[ "$HEALTH" == *"healthy"* ]]; then
                success "Health check passed: $HEALTH"
            else
                warn "Health check returned: $HEALTH"
            fi

            # Enable on boot
            if [[ "$AUTO_START" == "true" ]] || [[ "$NON_INTERACTIVE" == "true" ]]; then
                sudo systemctl enable "$SERVICE_NAME"
                success "Service enabled on boot"
            else
                echo ""
                ask "Enable service on boot? (y/n)"
                read -r ENABLE_BOOT
                [[ "$ENABLE_BOOT" == "y" ]] && sudo systemctl enable "$SERVICE_NAME"
            fi
        else
            echo ""
            error "Service failed to start. Check logs:"
            echo "  sudo journalctl -u $SERVICE_NAME -n 50"
        fi
    fi

    echo ""
    success "xrok server setup complete!"
}

#######################################
# Main
#######################################
main() {
    echo ""
    echo "╔═══════════════════════════════════════════════════════╗"
    echo "║           xrok Server Setup Script                    ║"
    echo "║           Domain: $DOMAIN"
    echo "╚═══════════════════════════════════════════════════════╝"
    echo ""

    install_dependencies
    install_go
    setup_repository
    build_xrok
    check_dns
    setup_certificates || true
    create_service
    configure_firewall
    setup_cert_renewal
    show_summary
}

main "$@"
