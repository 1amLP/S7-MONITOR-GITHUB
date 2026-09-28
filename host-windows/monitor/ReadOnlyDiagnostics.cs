using System;
using System.ComponentModel;
using System.Diagnostics;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;

// Reads diagnostics and optionally operates the engineering-only UI endpoint.
// No host-status writes, resets, shell commands or pipe-policy changes.
public static class S7ReadOnlyDiagnostics
{
    public static string ExpectedSerial { get; set; } = "";
    [StructLayout(LayoutKind.Sequential, Pack = 1)]
    struct Setup { public byte Type, Request; public ushort Value, Index, Length; }
    [StructLayout(LayoutKind.Sequential)]
    struct Overlapped { public UIntPtr Internal, InternalHigh; public uint Offset, OffsetHigh; public IntPtr Event; }
    [StructLayout(LayoutKind.Sequential, Pack = 1)]
    struct InterfaceDescriptor { public byte Length, Type, Number, Alternate, Endpoints, Class, Subclass, Protocol, String; }
    [StructLayout(LayoutKind.Sequential)]
    struct Pipe { public int Type; public byte Id; public ushort Packet; public byte Interval; }

    [DllImport("cfgmgr32.dll", CharSet = CharSet.Unicode)]
    static extern uint CM_Get_Device_Interface_List_SizeW(out uint size, ref Guid guid, string device, uint flags);
    [DllImport("cfgmgr32.dll", CharSet = CharSet.Unicode)]
    static extern uint CM_Get_Device_Interface_ListW(ref Guid guid, string device, [Out] char[] list, uint size, uint flags);
    [DllImport("cfgmgr32.dll", CharSet = CharSet.Unicode)]
    static extern uint CM_Locate_DevNodeW(out uint node, string device, uint flags);
    [DllImport("cfgmgr32.dll")]
    static extern uint CM_Get_Parent(out uint parent, uint child, uint flags);
    [DllImport("cfgmgr32.dll", CharSet = CharSet.Unicode)]
    static extern uint CM_Get_Device_IDW(uint node, StringBuilder id, uint size, uint flags);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    static extern IntPtr CreateFileW(string name, uint access, uint share, IntPtr security, uint creation, uint flags, IntPtr template);
    [DllImport("kernel32.dll", SetLastError = true)]
    static extern IntPtr CreateEventW(IntPtr attributes, bool manual, bool initial, IntPtr name);
    [DllImport("kernel32.dll", SetLastError = true)]
    static extern bool CloseHandle(IntPtr handle);
    [DllImport("kernel32.dll", SetLastError = true)]
    static extern uint WaitForSingleObject(IntPtr handle, uint timeout);
    [DllImport("kernel32.dll", SetLastError = true)]
    static extern bool CancelIoEx(IntPtr file, IntPtr overlapped);
    [DllImport("winusb.dll", SetLastError = true)]
    static extern bool WinUsb_Initialize(IntPtr file, out IntPtr usb);
    [DllImport("winusb.dll", SetLastError = true)]
    static extern bool WinUsb_Free(IntPtr usb);
    [DllImport("winusb.dll", SetLastError = true)]
    static extern bool WinUsb_QueryInterfaceSettings(IntPtr usb, byte alternate, out InterfaceDescriptor descriptor);
    [DllImport("winusb.dll", SetLastError = true)]
    static extern bool WinUsb_QueryPipe(IntPtr usb, byte alternate, byte index, out Pipe pipe);
    [DllImport("winusb.dll", SetLastError = true)]
    static extern bool WinUsb_ReadPipe(IntPtr usb, byte pipe, IntPtr data, uint size, IntPtr transferred, IntPtr overlapped);
    [DllImport("winusb.dll", SetLastError = true)]
    static extern bool WinUsb_WritePipe(IntPtr usb, byte pipe, IntPtr data, uint size, IntPtr transferred, IntPtr overlapped);
    [DllImport("winusb.dll", SetLastError = true)]
    static extern bool WinUsb_ControlTransfer(IntPtr usb, Setup setup, IntPtr data, uint size, IntPtr transferred, IntPtr overlapped);
    [DllImport("winusb.dll", SetLastError = true)]
    static extern bool WinUsb_GetOverlappedResult(IntPtr usb, IntPtr overlapped, out uint transferred, bool wait);

    static Exception Error(string operation) { return new Win32Exception(Marshal.GetLastWin32Error(), operation); }

    static string Find()
    {
        Guid guid = new Guid("79d1b2bc-ab1a-4ab1-ae69-6fab83ac15b0");
        string device = "USB\\VID_04E8&PID_A7C1&MI_";
        // Pin the current physical PnP parent before sending interface requests.
        uint size;
        uint result = CM_Get_Device_Interface_List_SizeW(out size, ref guid, null, 0);
        if (result != 0 || size < 2 || size > 65536) throw new IOException("S7 interface list unavailable: " + result);
        char[] list = new char[size];
        result = CM_Get_Device_Interface_ListW(ref guid, null, list, size, 0);
        if (result != 0) throw new IOException("S7 interface list changed: " + result);
        string selected = null;
        foreach (string path in new string(list).Split('\0'))
        {
            if (!path.StartsWith("\\\\?\\" + device.Replace('\\', '#'), StringComparison.OrdinalIgnoreCase)) continue;
            if (selected != null) throw new IOException("Ambiguous S7 interface");
            selected = path;
        }
        if (selected == null) throw new IOException("S7 engineering diagnostics interface not present; monitor interface is not used");
        RequirePhysicalParent(selected);
        return selected;
    }

    static void RequirePhysicalParent(string path)
    {
        string[] pieces = path.Split('#');
        if (pieces.Length != 4) throw new IOException("Invalid diagnostic interface path");
        uint node, parent;
        string id = "USB\\" + pieces[1] + "\\" + pieces[2];
        if (CM_Locate_DevNodeW(out node, id, 0) != 0 || CM_Get_Parent(out parent, node, 0) != 0)
            throw new IOException("S7 physical parent unavailable");
        StringBuilder parentId = new StringBuilder(512);
        if (ExpectedSerial.Length != 18) throw new IOException("Device serial is not configured");
        foreach (char digit in ExpectedSerial) if (!Uri.IsHexDigit(digit)) throw new IOException("Invalid device serial");
        if (CM_Get_Device_IDW(parent, parentId, 512, 0) != 0 || !String.Equals(parentId.ToString(), "USB\\VID_04E8&PID_A7C1\\" + ExpectedSerial, StringComparison.OrdinalIgnoreCase))
            throw new IOException("Operation refused for a different physical device");
    }

    // Descriptor requests are IN only and share the same bounded I/O lifetime.
    static byte[] ReadTransfer(IntPtr file, IntPtr usb, Setup setup, ref bool quarantined, byte[] output = null, byte pipe = 0, uint timeout = 1000)
    {
	    string request = String.Format("USB pipe={0:X2} type={1:X2} request={2:X2} value={3} index={4} length={5}", pipe, setup.Type, setup.Request, setup.Value, setup.Index, setup.Length);
        IntPtr buffer = Marshal.AllocHGlobal(setup.Length);
        IntPtr ov = Marshal.AllocHGlobal(Marshal.SizeOf(typeof(Overlapped)));
        IntPtr ev = CreateEventW(IntPtr.Zero, true, false, IntPtr.Zero);
        bool pending = false;
        try
        {
            if (ev == IntPtr.Zero) throw Error("CreateEvent");
            if (output != null)
            {
                if (output.Length != setup.Length || (setup.Type & 0x80) != 0) throw new IOException("Invalid OUT packet");
                Marshal.Copy(output, 0, buffer, output.Length);
            }
            Marshal.StructureToPtr(new Overlapped { Event = ev }, ov, false);
            bool submitted = pipe == 0 ? WinUsb_ControlTransfer(usb, setup, buffer, setup.Length, IntPtr.Zero, ov) :
                output == null ? WinUsb_ReadPipe(usb, pipe, buffer, setup.Length, IntPtr.Zero, ov) : WinUsb_WritePipe(usb, pipe, buffer, setup.Length, IntPtr.Zero, ov);
            int error = submitted ? 0 : Marshal.GetLastWin32Error();
            if (!submitted && error != 997) throw new Win32Exception(error, "Submit " + request);
            pending = true;
            if (WaitForSingleObject(ev, timeout) != 0)
            {
                CancelIoEx(file, ov);
                if (WaitForSingleObject(ev, 500) == 0)
                {
                    uint ignored;
                    bool done = WinUsb_GetOverlappedResult(usb, ov, out ignored, false);
                    pending = !done && Marshal.GetLastWin32Error() == 996;
                }
                throw new TimeoutException("S7 diagnostic timeout: " + request + "; no device reset attempted");
            }
            uint bytes;
            bool completed = WinUsb_GetOverlappedResult(usb, ov, out bytes, false);
            int completionError = completed ? 0 : Marshal.GetLastWin32Error();
            pending = !completed && completionError == 996;
            if (!completed) throw new Win32Exception(completionError, "Complete " + request);
            if (bytes > setup.Length) throw new IOException("Invalid USB read length");
            byte[] data = new byte[bytes];
            Marshal.Copy(buffer, data, 0, data.Length);
            return data;
        }
        finally
        {
            // A late completion still owns these pointers and handles. Keep them alive
            // until process exit instead of corrupting memory after a stuck cancellation.
            if (pending) quarantined = true;
            else
            {
                if (ev != IntPtr.Zero) CloseHandle(ev);
                Marshal.FreeHGlobal(ov);
                Marshal.FreeHGlobal(buffer);
            }
        }
    }

    static bool unavailable;
    public static string Read() { return new UTF8Encoding(false, true).GetString(Exchange(null)).Trim(); }

    // Explicit recovery for an abandoned bulk snapshot. It touches only the
    // diagnostics IN endpoint; no device reset or monitor control is sent.
    public static long Drain()
    {
        lock (typeof(S7ReadOnlyDiagnostics))
        {
            if (unavailable) throw new IOException("Previous diagnostic request remains owned by Windows; reopen this diagnostic process");
            IntPtr file = CreateFileW(Find(), 0xc0000000, 3, IntPtr.Zero, 3, 0x40000000, IntPtr.Zero);
            if (file == new IntPtr(-1)) throw Error("Open native S7");
            IntPtr usb = IntPtr.Zero;
            bool quarantined = false;
            try
            {
                if (!WinUsb_Initialize(file, out usb)) throw Error("Initialize S7 WinUSB");
                InterfaceDescriptor descriptor;
                if (!WinUsb_QueryInterfaceSettings(usb, 0, out descriptor)) throw Error("Query S7 interface");
                if (descriptor.Class != 0xff || descriptor.Subclass != 0x53 || descriptor.Protocol != 0x73 || descriptor.Endpoints != 2)
                    throw new IOException("Not the native S7 bulk diagnostics interface");
                byte input = 0;
                for (byte i = 0; i < 2; ++i)
                {
                    Pipe pipe;
                    if (!WinUsb_QueryPipe(usb, 0, i, out pipe)) throw Error("Query diagnostic bulk pipe");
                    if (pipe.Type != 2 || (pipe.Id & 15) == 0 || (pipe.Packet != 64 && pipe.Packet != 512))
                        throw new IOException("Invalid diagnostic bulk endpoint");
                    if ((pipe.Id & 0x80) != 0) input = pipe.Id;
                }
                if (input == 0) throw new IOException("Missing diagnostic bulk IN endpoint");
                long drained = 0;
                Stopwatch deadline = Stopwatch.StartNew();
                while (drained < 32L * 1024 * 1024 && deadline.ElapsedMilliseconds < 20000)
                {
                    try
                    {
                        byte[] part = ReadTransfer(file, usb, new Setup { Type = 0x80, Length = 32768 }, ref quarantined, null, input, 1000);
                        if (part.Length == 0) return drained;
                        drained += part.Length;
                    }
                    catch (TimeoutException)
                    {
                        if (quarantined) throw;
                        return drained;
                    }
                }
                throw new IOException("Diagnostic drain limit exceeded");
            }
            finally
            {
                if (quarantined) unavailable = true;
                else { if (usb != IntPtr.Zero) WinUsb_Free(usb); CloseHandle(file); }
            }
        }
    }

    public static byte[] Menu(uint kind, int x, int y, uint capture)
    {
        if (kind < 1 || kind > 8) throw new ArgumentException("Unknown menu command");
        if (kind == 8 && (x != 0x52564352 || y != 0 || capture != 0)) throw new ArgumentException("Recovery confirmation missing");
        if (kind == 5 && (x < 0 || x > 32767 || y < 0 || y > 32767 || capture == 0)) throw new ArgumentException("Invalid normalized tap");
        if (kind == 6 && (x == 0 || x < -720 || x > 720 || y != 0 || capture == 0)) throw new ArgumentException("Invalid scroll");
        if (kind != 5 && kind != 6 && kind != 8 && (x != 0 || y != 0 || capture != 0)) throw new ArgumentException("Unexpected menu arguments");
        return Exchange(MakeCommand(kind, x, y, capture));
    }

    public static byte[] PackageInfo() { return Exchange(MakeCommand(10, 0, 0, 0)); }
    public static byte[] PackageChunk(uint release, int offset, int count)
    {
        if(release == 0 || offset < 0 || count < 1 || count > 65536 || offset > 134217728 - count)
            throw new ArgumentException("Invalid package range");
        return Exchange(MakeCommand(11, offset, count, release));
    }

    public static void DownloadPackage(uint release, long size, string expectedHash, string path)
    {
        if(release==0||size<1||size>134217728||expectedHash==null||expectedHash.Length!=64)
            throw new ArgumentException("Invalid package identity");
        Stopwatch deadline=Stopwatch.StartNew();
        using(var output=new FileStream(path,FileMode.CreateNew,FileAccess.Write,FileShare.None))
        using(var hash=System.Security.Cryptography.SHA256.Create())
        {
            for(int offset=0;offset<size;)
            {
                if(deadline.ElapsedMilliseconds>120000)throw new TimeoutException("Package download deadline");
                int amount=(int)Math.Min(65536,size-offset);
                byte[] packet=PackageChunk(release,offset,amount);
                int meta=checked((int)BitConverter.ToUInt32(packet,12));
                int bytes=checked((int)BitConverter.ToUInt32(packet,16));
                if(BitConverter.ToUInt32(packet,8)!=2||bytes!=amount||packet.Length!=32+meta+bytes)
                    throw new IOException("Package chunk rejected or truncated");
                // Packet CRC protects transfer; full SHA256 pins the immutable
                // package across all chunks and reconnects. No partial is run.
                output.Write(packet,32+meta,bytes);
                hash.TransformBlock(packet,32+meta,bytes,null,0);offset+=bytes;
            }
            hash.TransformFinalBlock(new byte[0],0,0);
            string actual=BitConverter.ToString(hash.Hash).Replace("-","").ToLowerInvariant();
            if(!String.Equals(actual,expectedHash,StringComparison.Ordinal))throw new IOException("Downloaded package SHA256 mismatch");
            output.Flush(true);
        }
    }

    static byte[] MakeCommand(uint kind, int x, int y, uint capture)
    {
        byte[] packet = new byte[24];
        Encoding.ASCII.GetBytes("UI01").CopyTo(packet, 0);
        uint sequence;
        using (var random = System.Security.Cryptography.RandomNumberGenerator.Create())
        {
            byte[] seed = new byte[4];
            do { random.GetBytes(seed); sequence = BitConverter.ToUInt32(seed, 0); } while (sequence == 0);
        }
        BitConverter.GetBytes(sequence).CopyTo(packet, 4);
        BitConverter.GetBytes(kind).CopyTo(packet, 8);
        BitConverter.GetBytes(x).CopyTo(packet, 12);
        BitConverter.GetBytes(y).CopyTo(packet, 16);
        BitConverter.GetBytes(capture).CopyTo(packet, 20);
        return packet;
    }

    static byte[] BulkExchange(IntPtr file, IntPtr usb, byte[] command, ref bool quarantined)
    {
        byte input = 0, output = 0;
        for (byte i = 0; i < 2; ++i)
        {
            Pipe pipe;
            if (!WinUsb_QueryPipe(usb, 0, i, out pipe)) throw Error("Query diagnostic bulk pipe");
            if (pipe.Type != 2 || (pipe.Id & 15) == 0 || (pipe.Packet != 64 && pipe.Packet != 512)) throw new IOException("Invalid diagnostic bulk endpoint");
            if ((pipe.Id & 0x80) != 0) { if (input != 0) throw new IOException("Duplicate IN pipe"); input = pipe.Id; }
            else { if (output != 0) throw new IOException("Duplicate OUT pipe"); output = pipe.Id; }
        }
        if (input == 0 || output == 0) throw new IOException("Missing diagnostic bulk direction");
        bool json = command == null;
        if (json) command = MakeCommand(9, 0, 0, 0);
        var sent = ReadTransfer(file, usb, new Setup { Length = 24 }, ref quarantined, command, output, 2000);
        if (sent.Length != 24) throw new IOException("Short diagnostic request; not retried");
        byte[] header = ReadTransfer(file, usb, new Setup { Type = 0x80, Length = 32 }, ref quarantined, null, input, 10000);
        if (header.Length != 32 || Encoding.ASCII.GetString(header, 0, 4) != "UI01") throw new IOException("Stale or malformed diagnostic reply; drain required");
        uint state = BitConverter.ToUInt32(header, 8), meta = BitConverter.ToUInt32(header, 12), pixels = BitConverter.ToUInt32(header, 16);
        if ((state != 2 && state != 3) || meta > 262144 || pixels > 2560 * 2560 * 4) throw new IOException("Invalid diagnostic reply bounds; drain required");
        int total = checked((int)(meta + pixels));
        if (BitConverter.ToUInt32(header, 4) != BitConverter.ToUInt32(command, 4))
        {
            ReadBulkPayload(file, usb, input, ref quarantined, total, null);
            throw new IOException("Stale diagnostic reply drained; requested action was not confirmed");
        }
        if (meta > (json ? 262144 : 65536) || (json && pixels != 0)) throw new IOException("Invalid diagnostic reply bounds");
        byte[] data = new byte[32 + total]; header.CopyTo(data, 0);
        ReadBulkPayload(file, usb, input, ref quarantined, total, data);
        if (BitConverter.ToUInt32(header, 24) == 0x31435243 && PayloadCRC(data) != BitConverter.ToUInt32(header, 20))
            throw new IOException("Diagnostic payload CRC mismatch; capture rejected, drain required");
        if (!json) return data;
        byte[] text = new byte[meta]; Array.Copy(data, 32, text, 0, meta); return text;
    }

    static readonly uint[] crcTable = MakeCRCTable();
    static uint[] MakeCRCTable()
    {
        uint[] table = new uint[256];
        for (uint i = 0; i < table.Length; ++i)
        {
            uint c = i;
            for (int bit = 0; bit < 8; ++bit) c = (c >> 1) ^ ((c & 1) == 0 ? 0u : 0xedb88320u);
            table[i] = c;
        }
        return table;
    }
    static uint PayloadCRC(byte[] data)
    {
        uint crc = 0xffffffffu;
        for (int i = 32; i < data.Length; ++i) crc = crcTable[(crc ^ data[i]) & 255] ^ (crc >> 8);
        return ~crc;
    }

    static void ReadBulkPayload(IntPtr file, IntPtr usb, byte input, ref bool quarantined, int total, byte[] destination)
    {
        Stopwatch deadline = Stopwatch.StartNew();
        for (int offset = 0; offset < total; )
        {
            if (deadline.ElapsedMilliseconds > 20000) throw new TimeoutException("Bulk snapshot deadline");
            ushort amount = (ushort)Math.Min(32768, total - offset);
            byte[] part = ReadTransfer(file, usb, new Setup { Type = 0x80, Length = amount }, ref quarantined, null, input, 3000);
            if (part.Length == 0) throw new IOException("Empty bulk snapshot chunk");
            if (destination != null) part.CopyTo(destination, 32 + offset);
            offset += part.Length;
        }
    }

    // Standard, read-only GET_STATUS on EP0. Separates bulk transport failure
    // from a dead controller without changing configuration or resetting USB.
    public static string ProbeDeviceStatus()
    {
        IntPtr file=CreateFileW(Find(),0xc0000000,3,IntPtr.Zero,3,0x40000000,IntPtr.Zero);
        if(file==new IntPtr(-1))throw Error("Open S7 control probe");
        IntPtr usb=IntPtr.Zero;bool quarantined=false;
        try {
            if(!WinUsb_Initialize(file,out usb))throw Error("Initialize S7 control probe");
            byte[] data=ReadTransfer(file,usb,new Setup{Type=0x80,Request=0,Length=2},ref quarantined);
            if(data.Length!=2)throw new IOException("Short USB device status");
            return BitConverter.ToString(data);
        } finally {
            if(!quarantined){if(usb!=IntPtr.Zero)WinUsb_Free(usb);CloseHandle(file);}
        }
    }

    static byte[] Exchange(byte[] command)
    {
        lock (typeof(S7ReadOnlyDiagnostics))
        {
            if (unavailable) throw new IOException("Previous diagnostic request remains owned by Windows; reopen this diagnostic process");
            IntPtr file = CreateFileW(Find(), 0xc0000000, 3, IntPtr.Zero, 3, 0x40000000, IntPtr.Zero);
            if (file == new IntPtr(-1)) throw Error("Open native S7");
            IntPtr usb = IntPtr.Zero;
            bool quarantined = false;
            try
            {
                if (!WinUsb_Initialize(file, out usb)) throw Error("Initialize S7 WinUSB");
                InterfaceDescriptor descriptor;
                if (!WinUsb_QueryInterfaceSettings(usb, 0, out descriptor)) throw Error("Query S7 interface");
                if (descriptor.Class != 0xff || descriptor.Subclass != 0x53)
                    throw new IOException("Not the native S7 diagnostics interface");
                if (descriptor.Protocol == 0x73 && descriptor.Endpoints == 2) return BulkExchange(file, usb, command, ref quarantined);
                if (descriptor.Protocol != 0x72 || descriptor.Endpoints != 1) throw new IOException("Unsupported diagnostic protocol");
                if (command != null)
                {
                    ReadTransfer(file, usb, new Setup { Type = 0x41, Request = 0x56, Index = descriptor.Number, Length = 24 }, ref quarantined, command);
                    Stopwatch deadline = Stopwatch.StartNew();
                    byte[] header;
                    for (;;)
                    {
                        header = ReadTransfer(file, usb, new Setup { Type = 0xc1, Request = 0x55, Index = descriptor.Number, Length = 32 }, ref quarantined);
                        if (header.Length != 32 || Encoding.ASCII.GetString(header, 0, 4) != "UI01") throw new IOException("Invalid UI diagnostic header");
                        uint state = BitConverter.ToUInt32(header, 8);
                        if (BitConverter.ToUInt32(header, 4) == BitConverter.ToUInt32(command, 4) && (state == 2 || state == 3)) break;
                        if (deadline.ElapsedMilliseconds > 10000) throw new TimeoutException("Menu command not completed; not retried");
                        System.Threading.Thread.Sleep(100);
                    }
                    uint meta = BitConverter.ToUInt32(header, 12), pixels = BitConverter.ToUInt32(header, 16);
                    if (meta > 65536 || pixels > 2560 * 2560 * 4) throw new IOException("Invalid UI diagnostic extent");
                    int total = checked((int)(meta + pixels));
                    byte[] data = new byte[32 + total]; header.CopyTo(data, 0);
                    for (int offset = 0, page = 1; offset < total; ++page)
                    {
                        if (deadline.ElapsedMilliseconds > 30000) throw new TimeoutException("Capture transfer deadline");
                        ushort amount = (ushort)Math.Min(4096, total - offset);
                        byte[] part = ReadTransfer(file, usb, new Setup { Type = 0xc1, Request = 0x55, Index = descriptor.Number, Value = checked((ushort)page), Length = amount }, ref quarantined);
                        if (part.Length != amount) throw new IOException("Truncated UI diagnostic payload");
                        part.CopyTo(data, 32 + offset); offset += amount;
                    }
                    byte[] after = ReadTransfer(file, usb, new Setup { Type = 0xc1, Request = 0x55, Index = descriptor.Number, Length = 32 }, ref quarantined);
                    if (after.Length != 32 || BitConverter.ToUInt32(after, 4) != BitConverter.ToUInt32(header, 4)) throw new IOException("UI capture replaced during transfer");
                    return data;
                }
                using (MemoryStream result = new MemoryStream())
                {
                    Stopwatch timer = Stopwatch.StartNew();
                    for (ushort page = 0; page < 65; ++page)
                    {
                        if (timer.ElapsedMilliseconds > 10000) throw new TimeoutException("S7 diagnostic snapshot deadline");
                        byte[] data = ReadTransfer(file, usb, new Setup { Type = 0xc1, Request = 0x54, Index = descriptor.Number, Value = page, Length = 4096 }, ref quarantined);
                        if (result.Length == 262144 && data.Length == 1 && data[0] == 32) return result.ToArray();
                        if (result.Length + data.Length > 262144) throw new IOException("S7 diagnostic snapshot exceeds limit");
                        result.Write(data, 0, data.Length);
                        if (data.Length < 4096) return result.ToArray();
                    }
                    throw new IOException("Unterminated S7 diagnostic snapshot");
                }
            }
            finally
            {
                if (quarantined) unavailable = true;
                else { if (usb != IntPtr.Zero) WinUsb_Free(usb); CloseHandle(file); }
            }
        }
    }
}
