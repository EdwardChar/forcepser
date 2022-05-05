cmake_minimum_required(VERSION 3.0.0)

string(REGEX MATCH "[^/]+$" FILENAME "${url}")
set(ZIP_PATH "${local_dir}/${FILENAME}")
if(NOT EXISTS "${ZIP_PATH}")
  file(DOWNLOAD "${url}" "${ZIP_PATH}")
endif()
file(REMOVE_RECURSE "${local_dir}/${dir}")
file(MAKE_DIRECTORY "${local_dir}/${dir}")

if ("${charset}" STREQUAL "gbk")
  find_program(UNZIP unzip REQUIRED CMAKE_FIND_ROOT_PATH_BOTH)
  execute_process(
    COMMAND ${UNZIP} -O gbk ${ZIP_PATH}
    WORKING_DIRECTORY "${local_dir}/${dir}"
  )
else()
  execute_process(
    COMMAND ${CMAKE_COMMAND} -E tar xzvf ${ZIP_PATH}
    WORKING_DIRECTORY "${local_dir}/${dir}"
  )
endif()
