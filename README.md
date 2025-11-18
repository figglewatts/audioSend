# audioSend

Pair of Go programs to send audio from ALSA over TCP. Contains a server that broadcasts audio to clients.
Clients read audio from the TCP stream and play it over the default ALSA device.

I use this to send audio from a record player to the audio mixers of my computers.

## sender
`./sender :5000`

## receiver
`./receiver <ip-of-sender>:5000`