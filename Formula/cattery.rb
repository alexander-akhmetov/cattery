class Cattery < Formula
  desc "Agent state in the kitty tab bar, plus a picker for jumping between agents"
  homepage "https://github.com/alexander-akhmetov/cattery"
  url "https://github.com/alexander-akhmetov/cattery.git",
      tag:      "v0.1.0",
      revision: "20e87810ed1221d24a36b1e80696afb7e431e3d4",
      using:    :git
  license "MIT"
  head "https://github.com/alexander-akhmetov/cattery.git", branch: "main", using: :git

  depends_on "go" => :build

  # Homebrew stages the checkout without a .git directory, so the Makefile
  # cannot read the version from git. Pass the one Homebrew installed.
  def install
    system "make", "build", "VERSION=#{version}"
    bin.install "cattery"
  end

  # The binary alone does nothing: kitty only shows agent state once the four
  # kitty files are installed and kitty.conf loads them.
  def caveats
    <<~EOS
      Run the installer, then reload kitty:

        cattery setup

      An upgrade replaces the binary but not the installed kitty files. Run
      `cattery setup` again after every upgrade; the picker warns when the two
      have drifted apart.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/cattery -version")
    assert_match "list agent windows", shell_output("#{bin}/cattery --help 2>&1")
  end
end
