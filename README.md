# depaware
depaware makes you aware of your Go dependencies.

It generates a list of your dependencies which you check in to your repo:

https://github.com/tailscale/tailscale/blob/main/cmd/tailscaled/depaware.txt

Then you and others can easily see what your dependencies are, how
they vary by operating system (the letters L(inux), D(arwin),
W(indows) in the left column), and whether they use unsafe (bomb
icon).

Then you hook it up to your CI so it's a build breakage if they're not up to date:

https://github.com/tailscale/tailscale/blob/main/.github/workflows/depaware.yml

Then during code review you'll see in your review whether/how your
dependencies changed, and you can decide whether that's appropriate.

You'll probably want to pin a specific vesion of the depaware tool in your go.mod file
that survives a "go mod tidy". You can add a file like this to your project:

https://github.com/tailscale/tailscale/commit/7795fcf4649ce4ddc2a5b345cb56516fa161b4b3
