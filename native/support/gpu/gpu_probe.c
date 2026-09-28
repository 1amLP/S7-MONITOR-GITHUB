/* Non-flashing Mali/OpenCL probe. It allocates one 4 KiB ION buffer and may
 * initialize the GPU; it does not change the display or persistent storage. */
typedef unsigned int cl_uint;
typedef int cl_int;
typedef unsigned long size_t;
typedef void *cl_platform_id;
typedef void *cl_device_id;
typedef void (*context_notify_fn)(const char *, const void *, size_t, void *);

extern void *dlopen(const char *, int);
extern void *dlsym(void *, const char *);
extern const char *dlerror(void);
extern long write(int, const void *, size_t);
extern size_t strlen(const char *);
extern char *strstr(const char *, const char *);
extern int snprintf(char *, size_t, const char *, ...);
extern int open(const char *, int, ...);
extern int ioctl(int, unsigned long, ...);
extern int close(int);
extern unsigned int alarm(unsigned int);
extern void _exit(int) __attribute__((noreturn));

typedef cl_int (*get_platforms_fn)(cl_uint, cl_platform_id *, cl_uint *);
typedef cl_int (*get_devices_fn)(cl_platform_id, unsigned long, cl_uint, cl_device_id *, cl_uint *);
typedef cl_int (*get_device_info_fn)(cl_device_id, cl_uint, size_t, void *, size_t *);
typedef void *(*create_context_fn)(const long *, cl_uint, const cl_device_id *, context_notify_fn, void *, cl_int *);
typedef void *(*import_memory_fn)(void *, unsigned long, const long *, void *, size_t, cl_int *);
typedef cl_int (*release_object_fn)(void *);
typedef void *(*create_queue_fn)(void *, cl_device_id, unsigned long, cl_int *);
typedef void *(*create_buffer_fn)(void *, unsigned long, size_t, void *, cl_int *);
typedef cl_int (*fill_buffer_fn)(void *, void *, const void *, size_t, size_t, size_t, cl_uint, void *const *, void *);
typedef cl_int (*finish_fn)(void *);
typedef void *(*create_program_fn)(void *, cl_uint, const char **, const size_t *, cl_int *);
typedef cl_int (*build_program_fn)(void *, cl_uint, const cl_device_id *, const char *, void (*)(void *, void *), void *);
typedef cl_int (*get_build_info_fn)(void *, cl_device_id, cl_uint, size_t, void *, size_t *);
typedef void *(*create_kernel_fn)(void *, const char *, cl_int *);
typedef cl_int (*set_kernel_arg_fn)(void *, cl_uint, size_t, const void *);
typedef cl_int (*enqueue_kernel_fn)(void *, void *, cl_uint, const size_t *, const size_t *, const size_t *, cl_uint, void *const *, void *);

static const char fill_kernel[] =
	"__kernel void fill(__global uint *dst) { dst[get_global_id(0)] = 0xa55aa55au; }";

struct ion_allocation {
	unsigned long length, alignment;
	unsigned int heap_mask, flags;
	int handle;
	unsigned int padding;
};
struct ion_handle { int handle; };
struct ion_fd { int handle, fd; };

_Static_assert(sizeof(struct ion_allocation) == 32, "S7 ION ABI");

static void line(const char *s) {
	(void)write(1, s, strlen(s));
	(void)write(1, "\n", 1);
}

static void code(const char *name, int value) {
	char message[96];
	int n = snprintf(message, sizeof(message), "%s=%d\n", name, value);
	if (n > 0 && (size_t)n < sizeof(message)) (void)write(1, message, (size_t)n);
}

static void device_string(get_device_info_fn info, cl_device_id device, cl_uint key, const char *name) {
	char value[128] = {0};
	char message[160];
	size_t required = 0;
	if (info(device, key, 0, 0, &required) != 0 || required < 2 || required > sizeof(value) ||
	    info(device, key, sizeof(value), value, &required) != 0) {
		int n = snprintf(message, sizeof(message), "%s=UNAVAILABLE", name);
		if (n > 0 && (size_t)n < sizeof(message)) line(message);
		return;
	}
	value[sizeof(value) - 1] = 0;
	int n = snprintf(message, sizeof(message), "%s=%s", name, value);
	if (n > 0 && (size_t)n < sizeof(message)) line(message);
}

static int imported_kernel(void *library, void *context, cl_device_id device, void *queue, void *memory) {
	create_program_fn create_program = (create_program_fn)dlsym(library, "clCreateProgramWithSource");
	build_program_fn build_program = (build_program_fn)dlsym(library, "clBuildProgram");
	get_build_info_fn build_info = (get_build_info_fn)dlsym(library, "clGetProgramBuildInfo");
	create_kernel_fn create_kernel = (create_kernel_fn)dlsym(library, "clCreateKernel");
	set_kernel_arg_fn set_arg = (set_kernel_arg_fn)dlsym(library, "clSetKernelArg");
	enqueue_kernel_fn enqueue = (enqueue_kernel_fn)dlsym(library, "clEnqueueNDRangeKernel");
	release_object_fn release_program = (release_object_fn)dlsym(library, "clReleaseProgram");
	release_object_fn release_kernel = (release_object_fn)dlsym(library, "clReleaseKernel");
	finish_fn finish = (finish_fn)dlsym(library, "clFinish");
	if (!create_program || !build_program || !create_kernel || !set_arg || !enqueue ||
	    !release_program || !release_kernel || !finish) {
		line("GPU_NDRANGE_SYMBOLS=FAIL");
		return 14;
	}
	cl_int err = 0;
	void *program = create_program(context, 1, (const char *[]){fill_kernel}, 0, &err);
	if (!program || err != 0) {
		code("GPU_PROGRAM_CREATE_ERROR", err ? err : -1);
		return 14;
	}
	void *kernel = 0;
	err = build_program(program, 1, &device, 0, 0, 0);
	if (err != 0) {
		code("GPU_PROGRAM_BUILD_ERROR", err);
		if (build_info) {
			char build_log[512] = {0};
			if (build_info(program, device, 0x1183, sizeof(build_log), build_log, 0) == 0) {
				build_log[sizeof(build_log) - 1] = 0;
				line(build_log);
			}
		}
		goto done;
	}
	kernel = create_kernel(program, "fill", &err);
	if (!kernel || err != 0) {
		code("GPU_KERNEL_CREATE_ERROR", err ? err : -1);
		err = err ? err : -1;
		goto done;
	}
	err = set_arg(kernel, 0, sizeof(memory), &memory);
	if (err != 0) {
		code("GPU_KERNEL_ARG_ERROR", err);
		goto done;
	}
	const size_t work_items = 4096 / sizeof(cl_uint);
	err = enqueue(queue, kernel, 1, 0, &work_items, 0, 0, 0, 0);
	code("GPU_NDRANGE_ENQUEUE_RESULT", err);
	if (err == 0) {
		err = finish(queue);
		code("GPU_NDRANGE_FINISH_RESULT", err);
	}
done:
	code("GPU_NDRANGE_RESULT", err);
	if (kernel) (void)release_kernel(kernel);
	(void)release_program(program);
	return err == 0 ? 0 : 14;
}

static int probe(void) {
	(void)alarm(10);
	void *library = dlopen("libGLES_mali.so", 2);
	if (!library) {
		line("MALI_LIBRARY=FAIL");
		const char *reason = dlerror();
		if (reason) line(reason);
		return 2;
	}
	get_platforms_fn platforms = (get_platforms_fn)dlsym(library, "clGetPlatformIDs");
	get_devices_fn devices = (get_devices_fn)dlsym(library, "clGetDeviceIDs");
	get_device_info_fn info = (get_device_info_fn)dlsym(library, "clGetDeviceInfo");
	create_context_fn create_context = (create_context_fn)dlsym(library, "clCreateContext");
	import_memory_fn import_memory = (import_memory_fn)dlsym(library, "clImportMemoryARM");
	release_object_fn release_mem = (release_object_fn)dlsym(library, "clReleaseMemObject");
	release_object_fn release_context = (release_object_fn)dlsym(library, "clReleaseContext");
	create_queue_fn create_queue = (create_queue_fn)dlsym(library, "clCreateCommandQueue");
	create_buffer_fn create_buffer = (create_buffer_fn)dlsym(library, "clCreateBuffer");
	fill_buffer_fn fill_buffer = (fill_buffer_fn)dlsym(library, "clEnqueueFillBuffer");
	finish_fn finish = (finish_fn)dlsym(library, "clFinish");
	release_object_fn release_queue = (release_object_fn)dlsym(library, "clReleaseCommandQueue");
	if (!platforms || !devices || !info || !create_context || !import_memory || !release_mem ||
	    !release_context || !create_queue || !finish || !release_queue) {
		line("OPENCL_SYMBOLS=FAIL");
		return 3;
	}
	cl_platform_id platform[4] = {0};
	cl_uint count = 0;
	if (platforms(0, 0, &count) != 0 || count < 1 || count > 4 || platforms(4, platform, &count) != 0) {
		line("OPENCL_PLATFORM=FAIL");
		return 4;
	}
	cl_device_id device[4] = {0};
	cl_uint gpu_count = 0;
	if (devices(platform[0], 4, 4, device, &gpu_count) != 0 || gpu_count < 1 || gpu_count > 4) {
		line("OPENCL_GPU=FAIL");
		return 5;
	}
	char extensions[8192] = {0};
	size_t required = 0;
	if (info(device[0], 0x1030, 0, 0, &required) != 0 || required < 1 || required > sizeof(extensions) ||
	    info(device[0], 0x1030, sizeof(extensions), extensions, &required) != 0) {
		line("OPENCL_EXTENSIONS=FAIL");
		return 6;
	}
	extensions[sizeof(extensions) - 1] = 0;
	line("MALI_LIBRARY=OK");
	line("OPENCL_GPU=OK");
	device_string(info, device[0], 0x102f, "OPENCL_DEVICE_VERSION");
	device_string(info, device[0], 0x103d, "OPENCL_C_VERSION");
	if (!strstr(extensions, "cl_arm_import_memory_dma_buf")) {
		line("DMA_BUF_EXTENSION=NO");
		return 7;
	}
	line("DMA_BUF_EXTENSION=YES");
	int ion = open("/dev/ion", 2);
	if (ion < 0) {
		line("ION_OPEN=FAIL");
		return 8;
	}
	struct ion_allocation allocation = {4096, 4096, 1, 0, -1, 0};
	if (ioctl(ion, 0xc0204900UL, &allocation) != 0 || allocation.handle < 0) {
		line("ION_ALLOC=FAIL");
		(void)close(ion);
		return 9;
	}
	struct ion_fd shared = {allocation.handle, -1};
	int shared_ok = ioctl(ion, 0xc0084904UL, &shared) == 0 && shared.fd >= 0;
	struct ion_handle handle = {allocation.handle};
	int freed = ioctl(ion, 0xc0044901UL, &handle) == 0;
	(void)close(ion);
	if (!shared_ok || !freed) {
		line("ION_SHARE_OR_RELEASE=FAIL");
		if (shared.fd >= 0) (void)close(shared.fd);
		return 10;
	}
	line("ION_DMA_BUF=OK");
	cl_int err = 0;
	void *context = create_context(0, 1, device, 0, 0, &err);
	if (!context || err != 0) {
		code("OPENCL_CONTEXT_ERROR", err);
		(void)close(shared.fd);
		return 11;
	}
	const long properties[] = {0x40b2, 0x40b4, 0};
	void *memory = import_memory(context, 1, properties, &shared.fd, 4096, &err);
	if (!memory || err != 0) {
		code("DMA_BUF_IMPORT_ERROR", err);
		(void)release_context(context);
		(void)close(shared.fd);
		return 12;
	}
	line("DMA_BUF_IMPORT=OK");
	void *queue = create_queue(context, device[0], 0, &err);
	if (!queue || err != 0) {
		code("OPENCL_QUEUE_ERROR", err);
		(void)release_mem(memory);
		(void)release_context(context);
		(void)close(shared.fd);
		return 13;
	}
	unsigned int pattern = 0xa55aa55a;
	if (create_buffer && fill_buffer) {
		cl_int ordinary_error = 0;
		void *ordinary = create_buffer(context, 1, 4096, 0, &ordinary_error);
		if (ordinary) {
			if (ordinary_error == 0) {
				ordinary_error = fill_buffer(queue, ordinary, &pattern, sizeof(pattern), 0, 4096, 0, 0, 0);
				if (ordinary_error == 0) ordinary_error = finish(queue);
			}
			(void)release_mem(ordinary);
		}
		code("GPU_NORMAL_FILL_RESULT", ordinary_error == 0 && !ordinary ? -1 : ordinary_error);
	} else {
		line("GPU_NORMAL_FILL_RESULT=UNAVAILABLE");
	}
	if (fill_buffer) {
		err = fill_buffer(queue, memory, &pattern, sizeof(pattern), 0, 4096, 0, 0, 0);
		if (err == 0) err = finish(queue);
		code("GPU_FILL_RESULT", err);
	} else {
		line("GPU_FILL_RESULT=UNAVAILABLE");
	}
	int kernel_result = imported_kernel(library, context, device[0], queue, memory);
	(void)release_queue(queue);
	(void)release_mem(memory);
	(void)release_context(context);
	(void)close(shared.fd);
	line("GPU_SCANOUT=NOT_TESTED");
	return kernel_result;
}

#ifndef GPU_PROBE_TEST
#ifdef GPU_PROBE_HOST
int main(void) { return probe(); }
#else
void _start(void) { _exit(probe()); }
#endif
#endif
