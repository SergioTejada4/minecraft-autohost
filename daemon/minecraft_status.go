package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

func (d *daemon) handleNoHost(conn net.Conn, input io.Reader) {
	d.handleMinecraftStatus(conn, input)
}

func (d *daemon) handleMinecraftStatus(conn net.Conn, input io.Reader) {
	packetReader := newProtocolReader(input)
	state, err := packetReader.handshakeState()
	if err != nil {
		_ = conn.Close()
		return
	}
	if state != 1 {
		_ = conn.Close()
		return
	}
	requestID, requestPayload, err := packetReader.packet()
	if err != nil || requestID != 0 || len(requestPayload) != 0 {
		_ = conn.Close()
		return
	}
	motd := "AutoHost: world is idle"
	if d.store != nil {
		if settings := d.store.getSettings(); settings.MOTD != "" {
			motd = settings.MOTD
		}
	}
	icon, _ := d.faviconDataURL()
	d.playersMu.RLock()
	onlinePlayers := len(d.players)
	d.playersMu.RUnlock()
	if err := writeStatusResponse(conn, motd, icon, onlinePlayers); err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	packetID, payload, err := packetReader.packet()
	if err == nil && packetID == 1 && len(payload) == 8 {
		var pong bytes.Buffer
		writeVarInt(&pong, 1)
		_, _ = pong.Write(payload)
		_ = writePacket(conn, pong.Bytes())
	}
	_ = conn.Close()
}

type protocolReader struct {
	reader io.Reader
}

func readMinecraftHandshake(reader io.Reader) (int32, []byte, error) {
	length, err := readVarInt(reader)
	if err != nil || length < 1 || length > 64*1024 {
		return 0, nil, errors.New("invalid Minecraft handshake length")
	}
	packet := make([]byte, length)
	if _, err := io.ReadFull(reader, packet); err != nil {
		return 0, nil, err
	}
	body := bytes.NewReader(packet)
	packetID, err := readVarInt(body)
	if err != nil || packetID != 0 {
		return 0, nil, errors.New("expected Minecraft handshake packet")
	}
	if _, err := readVarInt(body); err != nil {
		return 0, nil, err
	}
	addressLength, err := readVarInt(body)
	if err != nil || addressLength < 0 || addressLength > 1024 {
		return 0, nil, errors.New("invalid Minecraft handshake address")
	}
	if _, err := io.CopyN(io.Discard, body, int64(addressLength)+2); err != nil {
		return 0, nil, err
	}
	state, err := readVarInt(body)
	if err != nil {
		return 0, nil, err
	}
	var raw bytes.Buffer
	writeVarInt(&raw, length)
	_, _ = raw.Write(packet)
	return state, raw.Bytes(), nil
}

func newProtocolReader(reader io.Reader) *protocolReader {
	return &protocolReader{reader: reader}
}

func (r *protocolReader) handshakeState() (int32, error) {
	packetID, payload, err := r.packet()
	if err != nil {
		return 0, err
	}
	if packetID != 0 {
		return 0, errors.New("expected handshake packet")
	}
	body := bytes.NewReader(payload)
	if _, err := readVarInt(body); err != nil {
		return 0, err
	}
	addressLength, err := readVarInt(body)
	if err != nil || addressLength < 0 || addressLength > 1024 {
		return 0, errors.New("invalid handshake address")
	}
	if _, err := io.CopyN(io.Discard, body, int64(addressLength)+2); err != nil {
		return 0, err
	}
	return readVarInt(body)
}

func (r *protocolReader) packet() (int32, []byte, error) {
	length, err := readVarInt(r.reader)
	if err != nil {
		return 0, nil, err
	}
	if length < 1 || length > 64*1024 {
		return 0, nil, errors.New("invalid Minecraft packet length")
	}
	packet := make([]byte, length)
	if _, err := io.ReadFull(r.reader, packet); err != nil {
		return 0, nil, err
	}
	body := bytes.NewReader(packet)
	packetID, err := readVarInt(body)
	if err != nil {
		return 0, nil, err
	}
	payload, err := io.ReadAll(body)
	return packetID, payload, err
}

func writeStatusResponse(writer io.Writer, motd, icon string, onlinePlayers int) error {
	status := map[string]any{
		"version":     map[string]any{"name": "1.21.1", "protocol": 767},
		"players":     map[string]any{"max": 20, "online": onlinePlayers, "sample": []any{}},
		"description": map[string]string{"text": motd},
	}
	if icon != "" {
		status["favicon"] = icon
	}
	jsonBytes, err := json.Marshal(status)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	writeVarInt(&body, 0)
	writeVarInt(&body, int32(len(jsonBytes)))
	_, _ = body.Write(jsonBytes)
	return writePacket(writer, body.Bytes())
}

func writeLoginDisconnect(writer io.Writer, reason string) error {
	jsonBytes, err := json.Marshal(map[string]string{"text": reason})
	if err != nil {
		return err
	}
	var body bytes.Buffer
	writeVarInt(&body, 0)
	writeVarInt(&body, int32(len(jsonBytes)))
	_, _ = body.Write(jsonBytes)
	return writePacket(writer, body.Bytes())
}

func writePacket(writer io.Writer, packet []byte) error {
	var output bytes.Buffer
	writeVarInt(&output, int32(len(packet)))
	_, _ = output.Write(packet)
	return writeAll(writer, output.Bytes())
}

func readVarInt(reader io.Reader) (int32, error) {
	var result int32
	for position := uint(0); position < 35; position += 7 {
		var current [1]byte
		if _, err := io.ReadFull(reader, current[:]); err != nil {
			return 0, err
		}
		result |= int32(current[0]&0x7f) << position
		if current[0]&0x80 == 0 {
			return result, nil
		}
	}
	return 0, errors.New("Minecraft VarInt exceeds 5 bytes")
}

func writeVarInt(writer io.Writer, value int32) {
	for value&^0x7f != 0 {
		_ = binaryWriteByte(writer, byte(value&0x7f|0x80))
		value = int32(uint32(value) >> 7)
	}
	_ = binaryWriteByte(writer, byte(value))
}

func binaryWriteByte(writer io.Writer, value byte) error {
	var buffer [1]byte
	buffer[0] = value
	return writeAll(writer, buffer[:])
}