{
  description = "Gas City (gc) orchestrator";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        gc = pkgs.buildGo127Module {
          pname = "gc";
          version = "dev";
          src = self;
          subPackages = [ "cmd/gc" ];
          # Update with: nix build .#gc 2>&1 | grep got:
          vendorHash = "sha256-EsYkvRNIV14ox/ryaGsnH7qr4rlafaUIy5w+GNNYuqI=";
          # Dolt's go-icu-regex links ICU through CGO.
          nativeBuildInputs = [ pkgs.pkg-config ];
          buildInputs = [ pkgs.icu ];
          env.CGO_ENABLED = "1";
          ldflags = [
            "-X main.version=dev"
            "-X main.commit=${self.shortRev or self.dirtyShortRev or "unknown"}"
            "-X main.date=${self.lastModifiedDate or "unknown"}"
          ];
          doCheck = false;
          meta.mainProgram = "gc";
        };
        default = gc;
      });
    };
}
